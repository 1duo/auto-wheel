// Package databento bulk batch path: minimal research quote+submit only.
//
// This file adds a separate bulk path for batch.submit_job without touching
// the existing streaming Range contract. Data downloading, normalization,
// and ledger completion remain a later phase and are NOT claimed here:
// a batch acknowledgement only reserves budget and records a job ID, it is
// not market data and never marks the ledger complete.
package databento

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// The study window is frozen. Existing full-window parent/stock queries and
// new slices of at most 24 hours are supported within this window.
const (
	BatchStart = "2026-04-06T00:00:00Z"
	BatchEnd   = "2026-10-03T00:00:00Z"
)

// Fixed batch format fields. They are part of the canonical query hash;
// the endpoint is part of the operation ID. There is no truncation and no
// limit/split_size on submit.
const (
	batchEncoding      = "dbn"
	batchCompression   = "zstd"
	batchStypeOut      = "instrument_id"
	batchSplitDuration = "day"
	batchDelivery      = "download"
	batchCostMode      = "historical"
)

const maxBatchResponseBytes = 1 << 20

var allowedBatchStates = []string{"queued", "processing", "done"}

// BatchQuery is the exact canonical bulk query. Symbol order is significant:
// any mutation (including reorder) changes the hash and invalidates quotes.
type BatchQuery struct {
	Dataset string   `json:"dataset"`
	Schema  string   `json:"schema"`
	Symbols []string `json:"symbols"`
	StypeIn string   `json:"stype_in"`
	Start   string   `json:"start"`
	End     string   `json:"end"`
}

// Validate rejects anything outside the bulk allowlist before any quote.
func (q BatchQuery) Validate() error {
	if len(q.Dataset) == 0 || len(q.Dataset) > 64 {
		return errors.New("databento batch dataset invalid")
	}
	for _, r := range q.Dataset {
		if !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return errors.New("databento batch dataset invalid")
		}
	}
	opra := q.Dataset == "OPRA.PILLAR"
	mini := q.Dataset == miniDataset
	summary := q.Dataset == "EQUS.SUMMARY"
	if !opra && !mini && !summary {
		return errors.New("databento batch scope is not cost-qualified")
	}
	if opra {
		if q.StypeIn == "parent" {
			if q.Schema != "definition" && q.Schema != "statistics" {
				return errors.New("databento batch scope is not cost-qualified")
			}
		} else if q.StypeIn == "raw_symbol" {
			if q.Schema != "definition" && q.Schema != "statistics" && q.Schema != "cmbp-1" {
				return errors.New("databento batch scope is not cost-qualified")
			}
		} else {
			return errors.New("databento batch scope is not cost-qualified")
		}
	} else if summary {
		if q.Schema != "ohlcv-1d" || q.StypeIn != "raw_symbol" {
			return errors.New("databento batch scope is not cost-qualified")
		}
	} else {
		if q.Schema != miniSchema || q.StypeIn != "raw_symbol" {
			return errors.New("databento batch scope is not cost-qualified")
		}
	}
	isRawOPRA := opra && q.StypeIn == "raw_symbol"
	if isRawOPRA {
		if len(q.Symbols) < 1 || len(q.Symbols) > 2000 {
			return errors.New("databento batch symbols invalid")
		}
	} else {
		if len(q.Symbols) < 1 || len(q.Symbols) > 15 {
			return errors.New("databento batch symbols invalid")
		}
	}
	seen := make(map[string]struct{}, len(q.Symbols))
	for _, s := range q.Symbols {
		if strings.TrimSpace(s) == "" || len(s) > 64 || strings.EqualFold(s, "ALL_SYMBOLS") {
			return errors.New("databento batch symbols invalid")
		}
		if _, dup := seen[s]; dup {
			return errors.New("databento batch symbols invalid")
		}
		seen[s] = struct{}{}
		if opra && !isRawOPRA {
			raw, ok := strings.CutSuffix(s, ".OPT")
			if !ok {
				return errors.New("databento batch scope is not cost-qualified")
			}
			if !slices.Contains(miniUniverse, raw) {
				return errors.New("databento batch scope is not cost-qualified")
			}
		} else if isRawOPRA {
			root, _, ok := parseOSIContract(s)
			if !ok {
				return errors.New("databento batch scope is not cost-qualified")
			}
			if !slices.Contains(miniUniverse, strings.TrimSpace(root)) {
				return errors.New("databento batch scope is not cost-qualified")
			}
			if s[13:21] == "00000000" {
				return errors.New("databento batch scope is not cost-qualified")
			}
		} else {
			if !slices.Contains(miniUniverse, s) {
				return errors.New("databento batch scope is not cost-qualified")
			}
		}
	}
	start, err := time.Parse(time.RFC3339Nano, q.Start)
	if err != nil {
		return errors.New("databento batch start time invalid")
	}
	end, err := time.Parse(time.RFC3339Nano, q.End)
	if err != nil {
		return errors.New("databento batch end time invalid")
	}
	if start.UTC().Format(time.RFC3339Nano) != q.Start {
		return errors.New("databento batch start time invalid")
	}
	if end.UTC().Format(time.RFC3339Nano) != q.End {
		return errors.New("databento batch end time invalid")
	}
	if !start.Before(end) {
		return errors.New("databento batch time range invalid")
	}
	batchStart, err := time.Parse(time.RFC3339Nano, BatchStart)
	if err != nil {
		return errors.New("databento batch window is not the frozen full range")
	}
	batchEnd, err := time.Parse(time.RFC3339Nano, BatchEnd)
	if err != nil {
		return errors.New("databento batch window is not the frozen full range")
	}
	if start.Before(batchStart) || end.After(batchEnd) {
		return errors.New("databento batch window is not the frozen full range")
	}
	// Official closing references are small daily bars for the same fixed
	// stock universe. This does not widen quote or tick acquisition windows.
	if summary {
		if end.Sub(start) > 62*24*time.Hour || start.UTC().Hour() != 0 || end.UTC().Hour() != 0 ||
			start.UTC().Minute() != 0 || end.UTC().Minute() != 0 || start.UTC().Second() != 0 || end.UTC().Second() != 0 ||
			start.Nanosecond() != 0 || end.Nanosecond() != 0 {
			return errors.New("databento summary daily window invalid")
		}
		return nil
	}
	if q.Start == BatchStart && q.End == BatchEnd {
		if isRawOPRA {
			return errors.New("databento batch time range invalid")
		}
		return nil
	}
	if end.Sub(start) > 24*time.Hour {
		return errors.New("databento batch time range invalid")
	}
	return nil
}

func (q BatchQuery) canonicalJSON() ([]byte, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	wire := struct {
		Dataset       string   `json:"dataset"`
		Schema        string   `json:"schema"`
		Symbols       []string `json:"symbols"`
		StypeIn       string   `json:"stype_in"`
		Start         string   `json:"start"`
		End           string   `json:"end"`
		Encoding      string   `json:"encoding"`
		Compression   string   `json:"compression"`
		StypeOut      string   `json:"stype_out"`
		SplitDuration string   `json:"split_duration"`
		SplitSymbols  bool     `json:"split_symbols"`
		PrettyPx      bool     `json:"pretty_px"`
		PrettyTs      bool     `json:"pretty_ts"`
		MapSymbols    bool     `json:"map_symbols"`
		Delivery      string   `json:"delivery"`
	}{
		Dataset:       q.Dataset,
		Schema:        q.Schema,
		Symbols:       slices.Clone(q.Symbols),
		StypeIn:       q.StypeIn,
		Start:         q.Start,
		End:           q.End,
		Encoding:      batchEncoding,
		Compression:   batchCompression,
		StypeOut:      batchStypeOut,
		SplitDuration: batchSplitDuration,
		SplitSymbols:  false,
		PrettyPx:      false,
		PrettyTs:      false,
		MapSymbols:    false,
		Delivery:      batchDelivery,
	}
	return json.Marshal(wire)
}

// Hash is hex SHA256 of the canonical batch query bytes including fixed
// format fields. Symbol order is significant.
func (q BatchQuery) Hash() (string, error) {
	raw, err := q.canonicalJSON()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// OperationID binds the canonical batch query to the batch endpoint.
func (q BatchQuery) OperationID(endpoint string) (string, error) {
	if endpoint != BatchSubmitEndpoint {
		return "", errors.New("databento batch endpoint unsupported")
	}
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

func (q BatchQuery) costForm() url.Values {
	return url.Values{
		"dataset":  {q.Dataset},
		"schema":   {q.Schema},
		"symbols":  {strings.Join(q.Symbols, ",")},
		"stype_in": {q.StypeIn},
		"start":    {q.Start},
		"end":      {q.End},
		"mode":     {batchCostMode},
	}
}

func (q BatchQuery) submitForm() url.Values {
	return url.Values{
		"dataset":        {q.Dataset},
		"symbols":        {strings.Join(q.Symbols, ",")},
		"schema":         {q.Schema},
		"start":          {q.Start},
		"end":            {q.End},
		"encoding":       {batchEncoding},
		"compression":    {batchCompression},
		"stype_in":       {q.StypeIn},
		"stype_out":      {batchStypeOut},
		"split_duration": {batchSplitDuration},
		"split_symbols":  {"false"},
		"pretty_px":      {"false"},
		"pretty_ts":      {"false"},
		"map_symbols":    {"false"},
		"delivery":       {batchDelivery},
	}
}

// validBatchQuote validates a batch quote without calling Quote.Valid,
// which only accepts the streaming range endpoint.
func validBatchQuote(q Quote, now time.Time) error {
	if len(q.OperationID) != 64 || len(q.QueryHash) != 64 || len(q.ResponseHash) != 64 {
		return errors.New("databento batch quote invalid")
	}
	if _, err := hex.DecodeString(q.OperationID); err != nil {
		return errors.New("databento batch quote invalid")
	}
	if _, err := hex.DecodeString(q.QueryHash); err != nil {
		return errors.New("databento batch quote invalid")
	}
	if _, err := hex.DecodeString(q.ResponseHash); err != nil {
		return errors.New("databento batch quote invalid")
	}
	if q.Endpoint != BatchSubmitEndpoint {
		return errors.New("databento batch quote endpoint mismatch")
	}
	if q.CostNano < 0 {
		return errors.New("databento batch quote cost invalid")
	}
	if q.QuotedAt.IsZero() {
		return errors.New("databento batch quote missing time")
	}
	if q.QuotedAt.After(now) {
		return errors.New("databento batch quote from the future")
	}
	if now.Sub(q.QuotedAt) > QuoteTTL {
		return errors.New("databento batch quote stale")
	}
	return nil
}

// GetBatchCost posts the exact canonical batch query to metadata.get_cost
// (free) with mode=historical and no limit, returning a Quote bound to the
// batch submit endpoint.
func (c *Client) GetBatchCost(ctx context.Context, q BatchQuery) (Quote, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return Quote{}, errors.New("databento client closed")
	}
	queryHash, err := q.Hash()
	if err != nil {
		return Quote{}, err
	}
	opID, err := q.OperationID(BatchSubmitEndpoint)
	if err != nil {
		return Quote{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, BaseURL+CostEndpoint, strings.NewReader(q.costForm().Encode()))
	if err != nil {
		return Quote{}, errors.New("databento batch quote request failed")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	c.auth(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Quote{}, errors.New("databento batch quote request failed")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBatchResponseBytes+1))
	if err != nil || len(body) > maxBatchResponseBytes {
		return Quote{}, errors.New("databento batch quote response too large")
	}
	sum := sha256.Sum256(body)
	responseHash := hex.EncodeToString(sum[:])
	if resp.StatusCode != http.StatusOK {
		return Quote{}, errors.New("databento batch quote request failed")
	}
	costStr, err := extractCost(body)
	if err != nil {
		return Quote{}, err
	}
	costNano, err := ParseNanoDollars(costStr)
	if err != nil {
		return Quote{}, errors.New("databento batch quote cost invalid")
	}
	now := time.Now().UTC()
	quote := Quote{
		OperationID:  opID,
		QueryHash:    queryHash,
		Endpoint:     BatchSubmitEndpoint,
		CostNano:     costNano,
		RawCost:      costStr,
		ResponseHash: responseHash,
		QuotedAt:     now,
	}
	if err := validBatchQuote(quote, now); err != nil {
		return Quote{}, err
	}
	c.quotes[opID] = quote
	return quote, nil
}

// BatchAcknowledgement records one accepted batch submit. It is not market
// data and never marks the ledger complete; the ledger row stays reserved.
type BatchAcknowledgement struct {
	OperationID  string    `json:"operation_id"`
	QueryHash    string    `json:"query_hash"`
	Endpoint     string    `json:"endpoint"`
	CostNano     int64     `json:"cost_nano"`
	ReservedNano int64     `json:"reserved_nano"`
	ResponseHash string    `json:"response_hash"`
	Bytes        int64     `json:"bytes"`
	StatusCode   int       `json:"status_code"`
	At           time.Time `json:"at"`
	JobID        string    `json:"job_id"`
	State        string    `json:"state"`
}

// SubmitBatch reserves before sending and submits one batch job with at most
// one POST. It requires an uncapped ledger, a fresh quote issued by this
// client for the exact canonical query, and no prior attempt for the same
// operation (even failed or ACK-lost). The ledger row remains reserved;
// Complete is never called on acknowledgement. Mismatches, HTTP errors, and
// cancellations preserve the hold.
func (c *Client) SubmitBatch(ctx context.Context, q BatchQuery, quote Quote) (BatchAcknowledgement, []byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return BatchAcknowledgement{}, nil, errors.New("databento client closed")
	}
	now := time.Now().UTC()
	if err := q.Validate(); err != nil {
		return BatchAcknowledgement{}, nil, err
	}
	if err := validBatchQuote(quote, now); err != nil {
		return BatchAcknowledgement{}, nil, err
	}
	queryHash, err := q.Hash()
	if err != nil {
		return BatchAcknowledgement{}, nil, err
	}
	opID, err := q.OperationID(BatchSubmitEndpoint)
	if err != nil {
		return BatchAcknowledgement{}, nil, err
	}
	if quote.QueryHash != queryHash || quote.OperationID != opID || quote.Endpoint != BatchSubmitEndpoint {
		return BatchAcknowledgement{}, nil, errors.New("databento batch query changed after quote")
	}
	if issued, ok := c.quotes[opID]; !ok || issued != quote {
		return BatchAcknowledgement{}, nil, errors.New("databento batch quote was not issued by this client")
	}
	if !c.ledger.IsUncapped() {
		return BatchAcknowledgement{}, nil, errors.New("databento batch requires uncapped ledger")
	}
	if _, exists := c.ledger.Find(opID); exists {
		return BatchAcknowledgement{}, nil, errors.New("databento batch already attempted; retry prohibited")
	}
	reservation, err := c.ledger.Reserve(opID, queryHash, BatchSubmitEndpoint, quote.CostNano, now, quote.ResponseHash)
	if err != nil {
		return BatchAcknowledgement{}, nil, err
	}
	attempt := BatchAcknowledgement{
		OperationID:  opID,
		QueryHash:    queryHash,
		Endpoint:     BatchSubmitEndpoint,
		CostNano:     quote.CostNano,
		ReservedNano: reservation.ReservedNano,
		At:           now,
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, BaseURL+BatchSubmitEndpoint, strings.NewReader(q.submitForm().Encode()))
	if err != nil {
		return attempt, nil, errors.New("databento batch submit failed; reservation remains charged")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	c.auth(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return attempt, nil, errors.New("databento batch submit failed; reservation remains charged")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBatchResponseBytes+1))
	if err != nil || len(body) > maxBatchResponseBytes {
		attempt.StatusCode = resp.StatusCode
		return attempt, nil, errors.New("databento batch submit failed; reservation remains charged")
	}
	sum := sha256.Sum256(body)
	attempt.ResponseHash = hex.EncodeToString(sum[:])
	attempt.Bytes = int64(len(body))
	attempt.StatusCode = resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		return attempt, bytes.Clone(body), errors.New("databento batch submit failed; reservation remains charged")
	}
	jobID, state, err := validateBatchSubmitBody(body, q)
	if err != nil {
		return attempt, bytes.Clone(body), err
	}
	attempt.JobID = jobID
	attempt.State = state
	return attempt, bytes.Clone(body), nil
}

func isBatchJobID(s string) bool {
	if len(s) < 1 || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// validateBatchSubmitBody checks the 1MiB acknowledgement: duplicate keys,
// trailing content, job ID, state, and exact returned scope including parsed
// timestamps. Limit must be null/missing (no truncation). Format and mode,
// when present, must match the fixed submit. Extra user_id/api_key aliases
// are allowed but never surfaced in errors.
func validateBatchSubmitBody(body []byte, q BatchQuery) (string, string, error) {
	fail := errors.New("databento batch acknowledgement invalid")
	if len(body) == 0 || len(body) > maxBatchResponseBytes {
		return "", "", fail
	}
	if rejectDuplicateKeys(body) != nil {
		return "", "", fail
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	var obj map[string]json.RawMessage
	if err := dec.Decode(&obj); err != nil {
		return "", "", fail
	}
	if dec.Decode(new(any)) != io.EOF {
		return "", "", fail
	}
	if obj == nil {
		return "", "", fail
	}
	var jobID string
	rawID, ok := obj["id"]
	if !ok || json.Unmarshal(rawID, &jobID) != nil || !isBatchJobID(jobID) {
		return "", "", fail
	}
	var state string
	rawState, ok := obj["state"]
	if !ok || json.Unmarshal(rawState, &state) != nil || !slices.Contains(allowedBatchStates, state) {
		return "", "", fail
	}
	var dataset, schema, stypeIn, stypeOut, encoding, compression, splitDuration, startStr, endStr string
	if raw, ok := obj["dataset"]; !ok || json.Unmarshal(raw, &dataset) != nil || dataset != q.Dataset {
		return "", "", fail
	}
	if raw, ok := obj["schema"]; !ok || json.Unmarshal(raw, &schema) != nil || schema != q.Schema {
		return "", "", fail
	}
	if raw, ok := obj["stype_in"]; !ok || json.Unmarshal(raw, &stypeIn) != nil || stypeIn != q.StypeIn {
		return "", "", fail
	}
	if raw, ok := obj["stype_out"]; !ok || json.Unmarshal(raw, &stypeOut) != nil || stypeOut != batchStypeOut {
		return "", "", fail
	}
	if raw, ok := obj["encoding"]; !ok || json.Unmarshal(raw, &encoding) != nil || encoding != batchEncoding {
		return "", "", fail
	}
	if raw, ok := obj["compression"]; !ok || json.Unmarshal(raw, &compression) != nil || compression != batchCompression {
		return "", "", fail
	}
	if raw, ok := obj["split_duration"]; !ok || json.Unmarshal(raw, &splitDuration) != nil || splitDuration != batchSplitDuration {
		return "", "", fail
	}
	if raw, ok := obj["start"]; !ok || json.Unmarshal(raw, &startStr) != nil {
		return "", "", fail
	}
	if raw, ok := obj["end"]; !ok || json.Unmarshal(raw, &endStr) != nil {
		return "", "", fail
	}
	wantStart, err := time.Parse(time.RFC3339Nano, q.Start)
	if err != nil {
		return "", "", fail
	}
	gotStart, err := time.Parse(time.RFC3339Nano, startStr)
	if err != nil || !gotStart.Equal(wantStart) {
		return "", "", fail
	}
	wantEnd, err := time.Parse(time.RFC3339Nano, q.End)
	if err != nil {
		return "", "", fail
	}
	gotEnd, err := time.Parse(time.RFC3339Nano, endStr)
	if err != nil || !gotEnd.Equal(wantEnd) {
		return "", "", fail
	}
	rawSymbols, ok := obj["symbols"]
	if !ok {
		return "", "", fail
	}
	var asString string
	if json.Unmarshal(rawSymbols, &asString) == nil {
		if asString != strings.Join(q.Symbols, ",") {
			return "", "", fail
		}
	} else {
		var asList []string
		if json.Unmarshal(rawSymbols, &asList) != nil || !slices.Equal(asList, q.Symbols) {
			return "", "", fail
		}
	}
	if raw, ok := obj["limit"]; ok {
		if strings.TrimSpace(string(raw)) != "null" {
			return "", "", fail
		}
	}
	if raw, ok := obj["cost_usd"]; ok {
		if strings.TrimSpace(string(raw)) != "null" {
			costStr, err := extractCost(raw)
			if err != nil {
				return "", "", fail
			}
			if _, err := ParseNanoDollars(costStr); err != nil {
				return "", "", fail
			}
		}
	}
	if raw, ok := obj["delivery"]; ok && strings.TrimSpace(string(raw)) != "null" {
		var delivery string
		if json.Unmarshal(raw, &delivery) != nil || delivery != batchDelivery {
			return "", "", fail
		}
	}
	if raw, ok := obj["mode"]; ok && strings.TrimSpace(string(raw)) != "null" {
		var mode string
		if json.Unmarshal(raw, &mode) != nil || mode != batchCostMode {
			return "", "", fail
		}
	}
	for _, key := range []string{"split_symbols", "pretty_px", "pretty_ts", "map_symbols"} {
		if raw, ok := obj[key]; ok && strings.TrimSpace(string(raw)) != "null" {
			var b bool
			if json.Unmarshal(raw, &b) == nil {
				if b {
					return "", "", fail
				}
				continue
			}
			var s string
			if json.Unmarshal(raw, &s) != nil || s != "false" {
				return "", "", fail
			}
		}
	}
	return jobID, state, nil
}
