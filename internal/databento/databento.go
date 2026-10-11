package databento

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Fixed production endpoints. There are no overrides; offline tests inject a
// fake RoundTripper while keeping these URLs.
const (
	BaseURL         = "https://hist.databento.com/v0"
	CostEndpoint    = "/metadata.get_cost"
	RangeEndpoint   = "/timeseries.get_range"
	ListingEndpoint = "/metadata.list_datasets"
)

// QuoteTTL bounds quote freshness. Downloads reject older quotes.
const QuoteTTL = 15 * time.Minute

const (
	maxCostResponseBytes    = 1 << 20
	maxListingResponseBytes = 1 << 20
	maxDownloadBytes        = 16 << 20
	maxKeyFileBytes         = 4096
	httpTimeout             = 90 * time.Second
	pilotMaxRecords         = 10_000
	pilotMaxRecordBytes     = int64(255 * 4)
)

// Query is the exact canonical billable query. Field order is canonical;
// symbol order is significant: any mutation (including reorder) changes the
// hash and invalidates prior quotes and reservations.
type Query struct {
	Dataset string   `json:"dataset"`
	Schema  string   `json:"schema"`
	Symbols []string `json:"symbols"`
	StypeIn string   `json:"stype_in"`
	Start   string   `json:"start"`
	End     string   `json:"end"`
	Limit   int      `json:"limit"`
}

var allowedSchemas = []string{
	"definition", "statistics", "cmbp-1", "trades", "tbbo", "tcbbo", "cbbo-1s", "cbbo-1m", "bbo-1s", "bbo-1m", "mbp-1",
	"ohlcv-1s", "ohlcv-1m", "ohlcv-1h", "ohlcv-1d",
}

var allowedStypeIn = []string{"raw_symbol", "instrument_id", "parent"}

// miniDataset and miniSchema identify the single bounded future-only stock
// alternative. miniUniverse is the frozen 15-member stock universe for that
// alternative; membership is exact case-sensitive and distinct.
const (
	miniDataset = "EQUS.MINI"
	miniSchema  = "mbp-1"
)

var miniUniverse = []string{
	"NFLX", "TSLA", "NVDA", "NBIS", "IREN", "NOW", "AMD", "AMZN", "GOOGL", "META", "MSFT", "PLTR", "UBER", "AVGO", "ORCL",
}

// Validate rejects ambiguous or unbounded queries before any quote.
func (q Query) Validate() error {
	if len(q.Dataset) == 0 || len(q.Dataset) > 64 {
		return errors.New("databento dataset invalid")
	}
	for _, r := range q.Dataset {
		if !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return errors.New("databento dataset invalid")
		}
	}
	if !slices.Contains(allowedSchemas, q.Schema) {
		return errors.New("databento schema unsupported")
	}
	if !slices.Contains(allowedStypeIn, q.StypeIn) {
		return errors.New("databento stype_in unsupported")
	}
	if len(q.Symbols) == 0 || len(q.Symbols) > 128 {
		return errors.New("databento symbols invalid")
	}
	for _, s := range q.Symbols {
		if strings.TrimSpace(s) == "" || len(s) > 64 || strings.EqualFold(s, "ALL_SYMBOLS") {
			return errors.New("databento symbols invalid")
		}
		for _, r := range s {
			if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' || r == ' ' || r == '/') {
				return errors.New("databento symbols invalid")
			}
		}
	}
	start, err := time.Parse(time.RFC3339Nano, q.Start)
	if err != nil {
		return errors.New("databento start time invalid")
	}
	end, err := time.Parse(time.RFC3339Nano, q.End)
	if err != nil {
		return errors.New("databento end time invalid")
	}
	if !start.Before(end) {
		return errors.New("databento time range invalid")
	}
	if end.Sub(start) > 366*24*time.Hour {
		return errors.New("databento time range too wide")
	}
	if start.Before(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) {
		return errors.New("databento start time invalid")
	}
	if q.Limit < 1 || q.Limit > 1000000 {
		return errors.New("databento limit invalid")
	}
	return nil
}

func (q Query) canonicalJSON() ([]byte, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	wire := struct {
		Dataset string   `json:"dataset"`
		Schema  string   `json:"schema"`
		Symbols []string `json:"symbols"`
		StypeIn string   `json:"stype_in"`
		Start   string   `json:"start"`
		End     string   `json:"end"`
		Limit   int      `json:"limit"`
	}{
		Dataset: q.Dataset,
		Schema:  q.Schema,
		Symbols: slices.Clone(q.Symbols),
		StypeIn: q.StypeIn,
		Start:   q.Start,
		End:     q.End,
		Limit:   q.Limit,
	}
	return json.Marshal(wire)
}

// Hash is hex SHA256 of the canonical query bytes.
func (q Query) Hash() (string, error) {
	raw, err := q.canonicalJSON()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// OperationID binds the canonical query to one source endpoint
// (query+source hash identity used by the ledger).
func (q Query) OperationID(endpoint string) (string, error) {
	raw, err := q.canonicalJSON()
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write(raw)
	h.Write([]byte{0})
	h.Write([]byte(endpoint))
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ParseNanoDollars parses an exact nonnegative decimal dollar string into
// integer nanodollars rounded UP, without binary floating point.
func ParseNanoDollars(s string) (int64, error) {
	const nanoPerDollar = int64(1_000_000_000)
	const maxInt64 = int64(1<<63 - 1)
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, errors.New("databento cost invalid")
	}
	if strings.HasPrefix(t, "-") || strings.HasPrefix(t, "+") {
		return 0, errors.New("databento cost invalid")
	}
	whole, frac, found := strings.Cut(t, ".")
	if whole == "" {
		return 0, errors.New("databento cost invalid")
	}
	if found && frac == "" {
		return 0, errors.New("databento cost invalid")
	}
	if strings.Contains(frac, ".") {
		return 0, errors.New("databento cost invalid")
	}
	if len(whole) > 19 || len(frac) > 32 {
		return 0, errors.New("databento cost invalid")
	}
	for _, c := range whole {
		if c < '0' || c > '9' {
			return 0, errors.New("databento cost invalid")
		}
	}
	for _, c := range frac {
		if c < '0' || c > '9' {
			return 0, errors.New("databento cost invalid")
		}
	}
	// Round UP beyond nanosecond precision.
	carry := false
	if len(frac) > 9 {
		extra := frac[9:]
		frac = frac[:9]
		if strings.Trim(extra, "0") != "" {
			carry = true
		}
	}
	for len(frac) < 9 {
		frac += "0"
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, errors.New("databento cost invalid")
	}
	var nano int64
	if frac != "" {
		n, err := strconv.ParseInt(frac, 10, 64)
		if err != nil || n < 0 || n >= nanoPerDollar {
			return 0, errors.New("databento cost invalid")
		}
		nano = n
	}
	if carry {
		if nano+1 >= nanoPerDollar {
			nano = 0
			if w == maxInt64 {
				return 0, errors.New("databento cost invalid")
			}
			w++
		} else {
			nano++
		}
	}
	if w < 0 || w > (maxInt64-nano)/nanoPerDollar {
		return 0, errors.New("databento cost invalid")
	}
	return w*nanoPerDollar + nano, nil
}

// LoadKey reads the private API key from an absolute regular non-symlink
// file owned by the current UID with mode 0600 inside a private parent
// directory. The returned bytes must be zeroed with ZeroKey when done.
// Errors never include key material.
func LoadKey(path string) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("databento key path must be absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, errors.New("databento key file unavailable")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("databento key path must not be a symlink")
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("databento key must be a regular file")
	}
	if info.Mode().Perm() != 0600 {
		return nil, errors.New("databento key file must have permissions 0600")
	}
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return nil, errors.New("databento key file unavailable")
	}
	if int(st.Uid) != os.Getuid() {
		return nil, errors.New("databento key file must be owned by the current user")
	}
	dir := filepath.Dir(path)
	dinfo, err := os.Lstat(dir)
	if err != nil || !dinfo.IsDir() || dinfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("databento key directory insecure")
	}
	if dinfo.Mode().Perm()&0077 != 0 {
		return nil, errors.New("databento key directory must be private")
	}
	var dirStat unix.Stat_t
	if unix.Lstat(dir, &dirStat) != nil || int(dirStat.Uid) != os.Getuid() {
		return nil, errors.New("databento key directory must be owned by current user")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("databento key file unavailable")
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	if err := unix.Fstat(fd, &st); err != nil || int(st.Uid) != os.Getuid() || st.Nlink != 1 || st.Mode&0777 != 0600 || st.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, errors.New("databento key file insecure")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxKeyFileBytes+1))
	if err != nil || len(raw) == 0 || len(raw) > maxKeyFileBytes {
		return nil, errors.New("databento key file invalid")
	}
	key := strings.TrimSpace(string(raw))
	if len(key) != 32 || !strings.HasPrefix(key, "db-") {
		return nil, errors.New("databento key invalid")
	}
	for _, c := range key[3:] {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return nil, errors.New("databento key invalid")
		}
	}
	return []byte(key), nil
}

// ZeroKey clears key material from memory.
func ZeroKey(b []byte) { clear(b) }

// Quote is a validated free cost estimate for one exact query and one
// billable endpoint. A zero cost still requires a valid quote.
type Quote struct {
	OperationID  string
	QueryHash    string
	Endpoint     string
	CostNano     int64
	RawCost      string
	ResponseHash string
	QuotedAt     time.Time
}

// Valid rejects absent, malformed, future-dated, or stale quotes.
func (q Quote) Valid(now time.Time) error {
	if len(q.OperationID) != 64 || len(q.QueryHash) != 64 || len(q.ResponseHash) != 64 {
		return errors.New("databento quote invalid")
	}
	if _, err := hex.DecodeString(q.OperationID); err != nil {
		return errors.New("databento quote invalid")
	}
	if _, err := hex.DecodeString(q.QueryHash); err != nil {
		return errors.New("databento quote invalid")
	}
	if _, err := hex.DecodeString(q.ResponseHash); err != nil {
		return errors.New("databento quote invalid")
	}
	if q.Endpoint != RangeEndpoint {
		return errors.New("databento quote endpoint mismatch")
	}
	if q.CostNano < 0 {
		return errors.New("databento quote cost invalid")
	}
	if q.QuotedAt.IsZero() {
		return errors.New("databento quote missing time")
	}
	if q.QuotedAt.After(now) {
		return errors.New("databento quote from the future")
	}
	if now.Sub(q.QuotedAt) > QuoteTTL {
		return errors.New("databento quote stale")
	}
	return nil
}

// Receipt proves one bounded download attempt. Bodies are hashed, never kept
// in errors or logs beyond the returned bytes.
type Receipt struct {
	OperationID    string
	QueryHash      string
	Endpoint       string
	CostNano       int64
	ReservedNano   int64
	ResponseSHA256 string
	Bytes          int64
	StatusCode     int
	At             time.Time
}

// Client performs free quotes/listings and cost-qualified downloads with at
// most one HTTP round trip per operation. No proxies, redirects, or retries.
type Client struct {
	mu         sync.Mutex
	key        []byte
	ledger     *Ledger
	httpClient *http.Client
	quotes     map[string]Quote
	closed     bool
}

func secureHTTPClient(rt http.RoundTripper) *http.Client {
	if rt != nil {
		return &http.Client{
			Transport:     rt,
			CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("databento redirects disabled") },
			Timeout:       httpTimeout,
		}
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:             nil,
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("databento redirects disabled") },
		Timeout:       httpTimeout,
	}
}

func validateKeyMaterial(key []byte) error {
	if len(key) != 32 || !strings.HasPrefix(string(key), "db-") {
		return errors.New("databento key invalid")
	}
	return nil
}

// NewClient copies key material into memory for Basic auth.
func NewClient(key []byte, ledger *Ledger) (*Client, error) {
	return NewClientWithTransport(key, ledger, nil)
}

// NewClientWithTransport injects a fake RoundTripper for offline tests.
// The request URL stays fixed to the production host.
func NewClientWithTransport(key []byte, ledger *Ledger, rt http.RoundTripper) (*Client, error) {
	if ledger == nil {
		return nil, errors.New("databento ledger required")
	}
	if err := validateKeyMaterial(key); err != nil {
		return nil, err
	}
	return &Client{key: bytes.Clone(key), ledger: ledger, httpClient: secureHTTPClient(rt), quotes: make(map[string]Quote)}, nil
}

// Close zeroes in-memory key material.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.key)
	clear(c.quotes)
	c.closed = true
	c.httpClient.CloseIdleConnections()
}

func (c *Client) auth(req *http.Request) {
	// Basic auth with the key as username and an empty password.
	// The key lives only in memory and never enters errors or logs.
	req.SetBasicAuth(string(c.key), "")
}

// GetCost posts the exact canonical query to metadata.get_cost (free) and
// returns a validated Quote bound to the billable range endpoint.
func (c *Client) GetCost(ctx context.Context, q Query) (Quote, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return Quote{}, errors.New("databento client closed")
	}
	queryHash, err := q.Hash()
	if err != nil {
		return Quote{}, err
	}
	opID, err := q.OperationID(RangeEndpoint)
	if err != nil {
		return Quote{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, BaseURL+CostEndpoint, strings.NewReader(q.form().Encode()))
	if err != nil {
		return Quote{}, errors.New("databento quote request failed")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	c.auth(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Quote{}, errors.New("databento quote request failed")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCostResponseBytes+1))
	if err != nil || len(body) > maxCostResponseBytes {
		return Quote{}, errors.New("databento quote response too large")
	}
	sum := sha256.Sum256(body)
	responseHash := hex.EncodeToString(sum[:])
	if resp.StatusCode != http.StatusOK {
		return Quote{}, errors.New("databento quote request failed")
	}
	costStr, err := extractCost(body)
	if err != nil {
		return Quote{}, err
	}
	costNano, err := ParseNanoDollars(costStr)
	if err != nil {
		return Quote{}, errors.New("databento quote cost invalid")
	}
	now := time.Now().UTC()
	quote := Quote{
		OperationID:  opID,
		QueryHash:    queryHash,
		Endpoint:     RangeEndpoint,
		CostNano:     costNano,
		RawCost:      costStr,
		ResponseHash: responseHash,
		QuotedAt:     now,
	}
	if err := quote.Valid(now); err != nil {
		return Quote{}, err
	}
	c.quotes[opID] = quote
	return quote, nil
}

func (q Query) form() url.Values {
	return url.Values{"dataset": {q.Dataset}, "schema": {q.Schema}, "symbols": {strings.Join(q.Symbols, ",")},
		"stype_in": {q.StypeIn}, "start": {q.Start}, "end": {q.End}, "limit": {strconv.Itoa(q.Limit)}}
}

func extractCost(body []byte) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var raw any
	if err := dec.Decode(&raw); err != nil {
		return "", errors.New("databento quote cost invalid")
	}
	if dec.Decode(new(any)) != io.EOF {
		return "", errors.New("databento quote cost invalid")
	}
	switch t := raw.(type) {
	case string:
		if strings.TrimSpace(t) == "" {
			return "", errors.New("databento quote cost invalid")
		}
		return strings.TrimSpace(t), nil
	case json.Number:
		return strings.TrimSpace(t.String()), nil
	default:
		return "", errors.New("databento quote cost invalid")
	}
}

// ListDatasets performs the free GET metadata listing with bounded bytes/time.
func (c *Client) ListDatasets(ctx context.Context) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errors.New("databento client closed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, BaseURL+ListingEndpoint, nil)
	if err != nil {
		return nil, errors.New("databento listing request failed")
	}
	req.Header.Set("Accept", "application/json")
	c.auth(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, errors.New("databento listing request failed")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxListingResponseBytes+1))
	if err != nil || len(body) > maxListingResponseBytes {
		return nil, errors.New("databento listing response too large")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("databento listing request failed")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var datasets []string
	if err := dec.Decode(&datasets); err != nil || dec.Decode(new(any)) != io.EOF {
		return nil, errors.New("databento listing response invalid")
	}
	if len(datasets) == 0 || len(datasets) > 1024 {
		return nil, errors.New("databento listing response invalid")
	}
	for _, d := range datasets {
		if d == "" || len(d) > 64 {
			return nil, errors.New("databento listing response invalid")
		}
	}
	return datasets, nil
}

// parseOSIContract validates a strict 21-byte ASCII OCC option symbol and returns
// its 6-byte root and 6-digit YYMMDD expiry. The root is uppercase alphanumeric
// space-padded on the right with no leading or interior spaces; expiry is a
// valid calendar date; byte 12 is C or P; the 8-byte strike is all digits.
func parseOSIContract(s string) (string, string, bool) {
	if len(s) != 21 {
		return "", "", false
	}
	for i := range len(s) {
		if s[i] < 32 || s[i] > 126 {
			return "", "", false
		}
	}
	root := s[:6]
	if root[0] == ' ' {
		return "", "", false
	}
	seenSpace := false
	for i := range 6 {
		c := root[i]
		if c == ' ' {
			seenSpace = true
			continue
		}
		if seenSpace {
			return "", "", false
		}
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return "", "", false
		}
	}
	expiry := s[6:12]
	for i := range 6 {
		if expiry[i] < '0' || expiry[i] > '9' {
			return "", "", false
		}
	}
	if _, err := time.Parse("20060102", "20"+expiry); err != nil {
		return "", "", false
	}
	if s[12] != 'C' && s[12] != 'P' {
		return "", "", false
	}
	for i := range 8 {
		if s[13+i] < '0' || s[13+i] > '9' {
			return "", "", false
		}
	}
	return root, expiry, true
}

// isCostQualifiedSymbolScope preserves single-symbol downloads and additionally
// allows a narrow selective batch: 2..128 raw_symbol contracts that are distinct
// strict 21-byte ASCII OSI symbols sharing one padded root and one YYMMDD expiry.
// Calls and puts may mix. Broader parents, instrument-ID lists, and arbitrary
// multi-root or multi-expiry batches remain unqualified.
func isCostQualifiedSymbolScope(q Query) bool {
	if len(q.Symbols) == 1 {
		return true
	}
	if q.StypeIn != "raw_symbol" {
		return false
	}
	if len(q.Symbols) < 2 || len(q.Symbols) > 128 {
		return false
	}
	seen := make(map[string]struct{}, len(q.Symbols))
	var prefix string
	for i, s := range q.Symbols {
		if _, _, ok := parseOSIContract(s); !ok {
			return false
		}
		if _, dup := seen[s]; dup {
			return false
		}
		seen[s] = struct{}{}
		cur := s[:12]
		if i == 0 {
			prefix = cur
		} else if cur != prefix {
			return false
		}
	}
	return true
}

// isMiniQualifiedSymbolScope allows only the bounded stock alternative: 1..15
// exact distinct uppercase members of the frozen miniUniverse with stype_in
// raw_symbol. Dataset/schema cross products are rejected by the GetRange scope
// gate; this helper checks only symbol scope. Lowercase, parent/instrument_id,
// duplicates, and other stocks remain unqualified.
func isMiniQualifiedSymbolScope(q Query) bool {
	if q.StypeIn != "raw_symbol" {
		return false
	}
	if len(q.Symbols) < 1 || len(q.Symbols) > 15 {
		return false
	}
	seen := make(map[string]struct{}, len(q.Symbols))
	for _, sym := range q.Symbols {
		if !slices.Contains(miniUniverse, sym) {
			return false
		}
		if _, dup := seen[sym]; dup {
			return false
		}
		seen[sym] = struct{}{}
	}
	return true
}

// GetRange downloads at most maxBytes for q only with a fresh matching quote.
// It reserves against the ledger BEFORE sending HTTP; any transport, status,
// or truncation failure stays charged/unknown with no automatic retry.
func (c *Client) GetRange(ctx context.Context, q Query, quote Quote, maxBytes int64) (Receipt, []byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return Receipt{}, nil, errors.New("databento client closed")
	}
	if maxBytes < 1 || maxBytes > maxDownloadBytes {
		return Receipt{}, nil, errors.New("databento download bound invalid")
	}
	now := time.Now().UTC()
	if err := q.Validate(); err != nil {
		return Receipt{}, nil, err
	}
	if err := quote.Valid(now); err != nil {
		return Receipt{}, nil, err
	}
	queryHash, err := q.Hash()
	if err != nil {
		return Receipt{}, nil, err
	}
	opID, err := q.OperationID(RangeEndpoint)
	if err != nil {
		return Receipt{}, nil, err
	}
	if quote.QueryHash != queryHash || quote.OperationID != opID || quote.Endpoint != RangeEndpoint {
		return Receipt{}, nil, errors.New("databento query changed after quote")
	}
	if issued, ok := c.quotes[opID]; !ok || issued != quote {
		return Receipt{}, nil, errors.New("databento quote was not issued by this client")
	}
	if _, exists := c.ledger.Find(opID); exists {
		return Receipt{}, nil, errors.New("databento download already attempted; retry prohibited")
	}
	// Cost qualification is limited to small OPRA samples plus one bounded
	// future-only stock alternative (EQUS.MINI mbp-1). A DBN record's
	// length is uint8 counts of 4-byte words (max 1020 bytes). With <=10,000
	// total records per request and <=24 hours, a fresh dataset/schema-qualified
	// fixed rate ceiling and a record-cap basis plus 10% margin conservatively
	// cover each request; adding symbols does not raise the per-request record
	// cap. A read limit alone cannot cap bytes the server already sent. Broader
	// downloads require separate qualification and do not qualify trading inputs
	// or a complete study.
	start, _ := time.Parse(time.RFC3339Nano, q.Start)
	end, _ := time.Parse(time.RFC3339Nano, q.End)
	if q.Limit > pilotMaxRecords || end.Sub(start) > 24*time.Hour {
		return Receipt{}, nil, errors.New("databento download scope is not cost-qualified")
	}
	opra := q.Dataset == "OPRA.PILLAR" && slices.Contains([]string{"definition", "statistics", "cmbp-1"}, q.Schema) && isCostQualifiedSymbolScope(q)
	mini := q.Dataset == miniDataset && q.Schema == miniSchema && isMiniQualifiedSymbolScope(q)
	if !opra && !mini {
		return Receipt{}, nil, errors.New("databento download scope is not cost-qualified")
	}
	if err := c.checkPilotUnitPriceFor(ctx, q.Dataset, q.Schema); err != nil {
		return Receipt{}, nil, err
	}
	basis, err := pilotReservationBasisFor(q.Dataset, q.Schema, q.Limit, quote.CostNano)
	if err != nil {
		return Receipt{}, nil, err
	}
	reservation, err := c.ledger.Reserve(opID, queryHash, RangeEndpoint, basis, now, quote.ResponseHash)
	if err != nil {
		return Receipt{}, nil, err
	}
	receipt := Receipt{OperationID: opID, QueryHash: queryHash, Endpoint: RangeEndpoint,
		CostNano: quote.CostNano, ReservedNano: reservation.ReservedNano, At: now}
	form := q.form()
	form.Set("encoding", "json")
	form.Set("compression", "none")
	form.Set("pretty_px", "false")
	form.Set("pretty_ts", "false")
	form.Set("map_symbols", "true")
	form.Set("stype_out", "instrument_id")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, BaseURL+RangeEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Receipt{}, nil, errors.New("databento download request failed")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	c.auth(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return receipt, nil, errors.New("databento download failed; reservation remains charged")
	}
	defer resp.Body.Close()
	receipt.StatusCode = resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		return receipt, nil, fmt.Errorf("databento download rejected HTTP %d; reservation remains charged", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return receipt, nil, errors.New("databento download failed; reservation remains charged")
	}
	if int64(len(body)) > maxBytes {
		return receipt, nil, errors.New("databento download truncated; reservation remains charged")
	}
	sum := sha256.Sum256(body)
	receipt = Receipt{
		OperationID:    reservation.OperationID,
		QueryHash:      queryHash,
		Endpoint:       RangeEndpoint,
		CostNano:       quote.CostNano,
		ReservedNano:   reservation.ReservedNano,
		ResponseSHA256: hex.EncodeToString(sum[:]),
		Bytes:          int64(len(body)),
		StatusCode:     resp.StatusCode,
		At:             now,
	}
	if err := c.ledger.Complete(opID, receipt.ResponseSHA256, receipt.Bytes); err != nil {
		return Receipt{}, nil, err
	}
	return receipt, body, nil
}

// pilotReservationBasis bounds only future requests using the already-qualified
// maximum record width and fixed rate ceiling. Decimal GB is conservative
// against GiB. The quote may raise the basis but never replace the record bound.
// Existing ledger entries remain immutable; ReserveAmount adds the 10% margin.
func pilotReservationBasis(schema string, limit int, quotedNano int64) (int64, error) {
	return pilotReservationBasisFor("OPRA.PILLAR", schema, limit, quotedNano)
}

// pilotReservationBasisFor is the dataset/schema-qualified shared basis used by
// both the inline gate and the reserve. The fixed ceiling never lowers for low
// observed rates; the quote may only raise the bound.
func pilotReservationBasisFor(dataset, schema string, limit int, quotedNano int64) (int64, error) {
	ceiling, err := pilotRateCeilingFor(dataset, schema)
	if err != nil {
		return 0, err
	}
	if limit < 1 || limit > pilotMaxRecords || quotedNano < 0 {
		return 0, errors.New("databento reservation inputs invalid")
	}
	const maxInt64 = int64(1<<63 - 1)
	if int64(limit) > maxInt64/pilotMaxRecordBytes {
		return 0, errors.New("databento record bound overflow")
	}
	recordBytes := int64(limit) * pilotMaxRecordBytes
	if recordBytes > maxInt64/ceiling {
		return 0, errors.New("databento record cost overflow")
	}
	numerator := recordBytes * ceiling
	const decimalGB = int64(1_000_000_000)
	bound := numerator / decimalGB
	if numerator%decimalGB != 0 {
		bound++
	}
	return max(quotedNano, bound), nil
}

// The gate and reservation use the same fixed ceiling. Fresh observed rates
// can reject a request but never lower its record-cap basis. These constants
// affect future entries only; recorded holds are never repriced or released.
// pilotRateCeilingFor is the shared dataset/schema-qualified fixed ceiling for
// the inline fresh unit-price gate and the checked basis. OPRA ceilings are
// unchanged ($6 definition, $12 statistics, $0.25 cmbp-1); EQUS.MINI mbp-1 is
// $2 (2_000_000_000 nano/decimalGB). Unknown pairs reject.
func pilotRateCeilingFor(dataset, schema string) (int64, error) {
	switch {
	case dataset == "OPRA.PILLAR" && schema == "definition":
		return 6_000_000_000, nil
	case dataset == "OPRA.PILLAR" && schema == "statistics":
		return 12_000_000_000, nil
	case dataset == "OPRA.PILLAR" && schema == "cmbp-1":
		return 250_000_000, nil
	case dataset == miniDataset && schema == miniSchema:
		return 2_000_000_000, nil
	default:
		return 0, errors.New("databento schema has no qualified fixed rate ceiling")
	}
}

func (c *Client) checkPilotUnitPriceFor(ctx context.Context, dataset, schema string) error {
	ceiling, err := pilotRateCeilingFor(dataset, schema)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, BaseURL+"/metadata.list_unit_prices?dataset="+dataset, nil)
	if err != nil {
		return errors.New("databento unit price request failed")
	}
	c.auth(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return errors.New("databento unit price request failed")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCostResponseBytes+1))
	if err != nil || len(body) > maxCostResponseBytes || resp.StatusCode != http.StatusOK {
		return errors.New("databento unit price unavailable")
	}
	var prices []map[string]json.RawMessage
	if rejectDuplicateKeys(body) != nil || json.Unmarshal(body, &prices) != nil {
		return errors.New("databento unit price invalid")
	}
	matched := 0
	var rate int64
	for _, row := range prices {
		var mode string
		if len(row) != 2 || json.Unmarshal(row["mode"], &mode) != nil || mode == "" {
			return errors.New("databento unit price row invalid")
		}
		if mode != "historical-streaming" {
			continue
		}
		matched++
		if matched > 1 {
			return errors.New("databento historical-streaming unit price ambiguous")
		}
		var unitPrices map[string]json.RawMessage
		if json.Unmarshal(row["unit_prices"], &unitPrices) != nil {
			return errors.New("databento unit price invalid")
		}
		value, ok := unitPrices[schema]
		if !ok {
			return errors.New("databento unit price unavailable")
		}
		text, err := extractCost(value)
		if err != nil {
			return errors.New("databento unit price invalid")
		}
		rate, err = ParseNanoDollars(text)
		if err != nil {
			return errors.New("databento unit price invalid")
		}
	}
	if matched != 1 || rate > ceiling {
		return errors.New("databento unit price exceeds qualified pilot ceiling or is unavailable")
	}
	return nil
}
