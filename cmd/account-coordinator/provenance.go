// Ownership preflight for the proposed shared coordinator.
//
// Read-only observer: it compares fresh broker facts (observed-account.json)
// against the TradePulse journal (pilot.sqlite3) and writes a private
// draft-preflight.json receipt. It never imports a ledger, mutates state,
// qualifies execution, or authorizes sharing one account.
//
// The deployed account-isolation guard stays in force: qualified and frozen
// are always false in the receipt while the original TradePulse writer is
// operational. Only a qualified single coordinator built on this preflight's
// evidence could ever relax that rule; this file is not that coordinator.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	aruntime "autowheel/internal/runtime"
	"autowheel/internal/sharedbroker"

	_ "modernc.org/sqlite"
)

// tpIntent mirrors the TradePulse order_intents row the peer owns.
type tpIntent struct {
	IntentKey     string
	Symbol        string
	PayloadHash   string
	PayloadJSON   string
	Status        string
	BrokerOrderID string
	Ambiguous     bool
}

// tpLifecycle mirrors the TradePulse lifecycles row the peer owns.
type tpLifecycle struct {
	Symbol          string
	StateJSON       string
	ReleaseHash     string
	UpdatedSequence int64
}

// journalSnapshot is a consistent read of the peer journal plus the
// maximum events sequence. It is evidence only, never an import.
type journalSnapshot struct {
	Orders      []tpIntent
	Lifecycles  []tpLifecycle
	MaxSequence int64
}

// preflightReceipt is the private draft evidence written to the coordinator
// state directory. Qualified and Frozen stay false while the original
// TradePulse writer is operational; no check in this file can set them true.
type preflightReceipt struct {
	At               time.Time `json:"at"`
	Qualified        bool      `json:"qualified"`
	Frozen           bool      `json:"frozen"`
	EvidenceComplete bool      `json:"ownership_evidence_complete"`
	FactsDigest      string    `json:"facts_digest"`
	FactsAt          time.Time `json:"facts_at,omitzero"`
	BrokerOrders     int       `json:"broker_orders"`
	JournalOrders    int       `json:"journal_orders"`
	Positions        int       `json:"positions"`
	Lifecycles       int       `json:"lifecycles"`
	TaxLots          int       `json:"tax_lots"`
	MaxSequence      int64     `json:"max_sequence"`
	JournalHash      string    `json:"journal_hash"`
	LifecycleHash    string    `json:"lifecycle_hash"`
	Blockers         []string  `json:"blockers"`
}

func brokerUUIDValid(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return false
			}
		}
	}
	return true
}

// internalDecimalID reports broker-foreign internal numbers such as the
// production 300000... tax-lot opener IDs. They must be reported, never
// fuzzy-mapped to UUIDs by amount or date.
func internalDecimalID(s string) bool {
	if len(s) < 6 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func decimalToMicros(v any) (int64, bool) {
	if text, ok := v.(string); ok {
		q, err := sharedbroker.Micros(text)
		return q, err == nil
	}
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	if q, err := sharedbroker.Micros(n.String()); err == nil {
		return q, true
	}
	// The peer persists f64 arithmetic residue. Accept only binary roundoff,
	// never a missing or changed microshare.
	value, err := strconv.ParseFloat(n.String(), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, false
	}
	q, err := sharedbroker.Micros(fmt.Sprintf("%.6f", value))
	actual, valid := new(big.Rat).SetString(n.String())
	if err != nil || !valid {
		return 0, false
	}
	tolerance := new(big.Rat).SetFloat64(2 * (math.Nextafter(value, math.Inf(1)) - value))
	if tolerance == nil || tolerance.Cmp(big.NewRat(1, 2_000_000)) >= 0 {
		return 0, false
	}
	difference := new(big.Rat).Sub(actual, big.NewRat(q, 1_000_000))
	return q, difference.Abs(difference).Cmp(tolerance) <= 0
}

func brokerSymbol(o sharedbroker.Object) string {
	if s := sharedbroker.Text(o, "symbol"); s != "" {
		return s
	}
	return sharedbroker.Text(o, "chain_symbol")
}

func brokerQtyMicros(o sharedbroker.Object) (int64, bool) {
	for _, key := range []string{"quantity", "cumulative_quantity", "processed_quantity"} {
		if q, ok := decimalToMicros(o[key]); ok {
			return q, true
		}
	}
	return 0, false
}

func lifecycleQtyMicros(raw string) (int64, error) {
	m, err := decodePeerObject(raw)
	if err != nil || sharedbroker.Text(m, "schema_version") != "PilotLifecycleV1" {
		return 0, errors.New("lifecycle schema missing or invalid")
	}
	q, ok := decimalToMicros(m["current_quantity"])
	if !ok {
		return 0, errors.New("lifecycle current_quantity missing or imprecise")
	}
	return q, nil
}

type payloadView struct {
	Key, Symbol, Side, Ref string
	Qty                    int64
	QtyOK                  bool
}

func decodePeerObject(raw string) (map[string]any, error) {
	var m map[string]any
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&m); err != nil {
		return nil, err
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, errors.New("trailing peer JSON")
	}
	return m, nil
}

func payloadFields(raw string) (payloadView, error) {
	m, err := decodePeerObject(raw)
	if err != nil {
		return payloadView{}, err
	}
	p := payloadView{Key: sharedbroker.Text(m, "intent_key"), Symbol: sharedbroker.Text(m, "symbol"), Side: sharedbroker.Text(m, "side"), Ref: sharedbroker.Text(m, "ref_id")}
	p.Qty, p.QtyOK = decimalToMicros(m["quantity"])
	if p.Key == "" || p.Symbol == "" || (p.Side != "buy" && p.Side != "sell") || !p.QtyOK || p.Qty <= 0 {
		return p, errors.New("exact intent identity incomplete")
	}
	return p, nil
}

// This is the deployed TradePulse deterministic_order_reference_id algorithm.
func peerOrderReference(key string) string {
	hex := []byte(fmt.Sprintf("%x", sha256.Sum256([]byte(key)))[:32])
	hex[12], hex[16] = '5', '8'
	return fmt.Sprintf("%s-%s-%s-%s-%s", hex[:8], hex[8:12], hex[12:16], hex[16:20], hex[20:])
}

// validateProvenance is the pure ownership gate. Every return value is a
// readiness blocker; an empty slice means the evidence joins cleanly, not
// that execution is qualified. Float tolerance is never authorization: broker
// microshare sums must match exactly, with only binary roundoff
// allowed in the peer's JSON float representations.
func validateProvenance(facts sharedbroker.Facts, snap journalSnapshot) []string {
	var blockers []string
	block := func(format string, args ...any) {
		blockers = append(blockers, fmt.Sprintf(format, args...))
	}

	journalByID := make(map[string]tpIntent)
	for _, j := range snap.Orders {
		if j.IntentKey == "" {
			block("journal order missing intent_key")
			continue
		}
		if j.Ambiguous {
			block("journal order %q is ambiguous", j.IntentKey)
		}
		if !sharedbroker.Terminal(j.Status) && j.Status != "review_rejected" {
			block("journal order %q status %q is not terminal", j.IntentKey, j.Status)
		}
		if !canonicalHash(j.PayloadHash) {
			block("journal order %q payload hash missing", j.IntentKey)
		} else if fmt.Sprintf("%x", sha256.Sum256([]byte(j.PayloadJSON))) != strings.ToLower(j.PayloadHash) {
			block("journal order %q payload hash mismatch", j.IntentKey)
		}
		p, err := payloadFields(j.PayloadJSON)
		if err != nil {
			block("journal order %q payload unreadable", j.IntentKey)
		} else {
			if p.Key != j.IntentKey {
				block("journal order %q payload key differs", j.IntentKey)
			}
			if p.Symbol != "" && j.Symbol != "" && strings.ToUpper(p.Symbol) != strings.ToUpper(j.Symbol) {
				block("journal order %q payload symbol %q mismatches row symbol %q", j.IntentKey, p.Symbol, j.Symbol)
			}
		}
		if strings.ToLower(j.Status) == "filled" {
			if !brokerUUIDValid(j.BrokerOrderID) {
				block("journal order %q filled without exact broker UUID (got %q)", j.IntentKey, j.BrokerOrderID)
			}
		} else if j.BrokerOrderID != "" && !brokerUUIDValid(j.BrokerOrderID) {
			block("journal order %q broker id %q is not a UUID", j.IntentKey, j.BrokerOrderID)
		}
		if j.BrokerOrderID != "" {
			if _, dup := journalByID[j.BrokerOrderID]; dup {
				block("duplicate journal broker order %q", j.BrokerOrderID)
			} else {
				journalByID[j.BrokerOrderID] = j
			}
		} else if j.Status != "rejected" && j.Status != "review_rejected" {
			block("journal order %q has no broker order id without a rejection", j.IntentKey)
		}
	}

	for _, o := range facts.Orders {
		id := sharedbroker.Text(o, "id")
		state := sharedbroker.Text(o, "state")
		if !brokerUUIDValid(id) || sharedbroker.Text(o, "symbol") == "" || sharedbroker.Text(o, "placed_agent") != "agentic" || sharedbroker.Text(o, "side") != "buy" && sharedbroker.Text(o, "side") != "sell" {
			block("broker equity order identity or agentic provenance missing")
			continue
		}
		if !sharedbroker.Terminal(state) {
			block("working equity order %q state %q blocks new risk", id, state)
		}
	}
	if len(facts.Options) > 0 {
		block("option position exposure present (%d rows)", len(facts.Options))
	}
	for _, o := range facts.OptionOrders {
		block("option order %q present; stock-only preflight cannot account for it", sharedbroker.Text(o, "id"))
	}
	for _, p := range facts.CryptoPositions {
		q, err := sharedbroker.Decimal(sharedbroker.Text(p, "quantity"))
		if err != nil || q.Sign() != 0 {
			block("crypto position exposure present")
			break
		}
	}
	for _, o := range facts.CryptoOrders {
		block("crypto order %q present; stock-only preflight cannot account for it", sharedbroker.Text(o, "id"))
	}

	brokerByID := make(map[string]sharedbroker.Object)
	for _, o := range facts.Orders {
		if id := sharedbroker.Text(o, "id"); id != "" {
			if _, dup := brokerByID[id]; dup {
				block("duplicate broker order %q", id)
			}
			brokerByID[id] = o
		}
	}
	for id, bo := range brokerByID {
		j, ok := journalByID[id]
		if !ok {
			block("unaccounted broker order %q; manual or foreign activity blocks new risk", id)
			continue
		}
		bSym, bSide := brokerSymbol(bo), strings.ToLower(sharedbroker.Text(bo, "side"))
		bQty, bOK := brokerQtyMicros(bo)
		bRef, bState := sharedbroker.Text(bo, "ref_id"), sharedbroker.Text(bo, "state")
		p, err := payloadFields(j.PayloadJSON)
		if err != nil {
			block("order %q payload unreadable", id)
			continue
		}
		if j.Symbol != "" && bSym != "" && strings.ToUpper(j.Symbol) != strings.ToUpper(bSym) {
			block("order %q symbol mismatch: journal %q vs broker %q", id, j.Symbol, bSym)
		}
		if p.Symbol != "" && bSym != "" && strings.ToUpper(p.Symbol) != strings.ToUpper(bSym) {
			block("order %q payload symbol %q mismatches broker %q", id, p.Symbol, bSym)
		}
		if strings.ToLower(j.Status) == "filled" || strings.ToLower(bState) == "filled" {
			if p.Side != "" && bSide != "" && p.Side != bSide {
				block("order %q side mismatch: journal %q vs broker %q", id, p.Side, bSide)
			}
			if !bOK || !p.QtyOK {
				block("order %q quantity unproven; missing data blocks", id)
			} else if bQty != p.Qty {
				block("order %q quantity mismatch: broker %d vs journal %d micros (existing peer floats explain, never authorize)", id, bQty, p.Qty)
			}
			if bRef != j.IntentKey && bRef != peerOrderReference(j.IntentKey) {
				block("broker order %q ref missing; identity unproven", id)
			} else if p.Ref != "" && p.Ref != bRef {
				block("order %q ref mismatch: broker %q vs payload %q", id, bRef, p.Ref)
			}
			if strings.ToLower(j.Status) == "filled" && strings.ToLower(bState) != "filled" {
				block("journal order %q filled but broker state is %q", id, bState)
			}
		}
	}
	for id, j := range journalByID {
		if _, ok := brokerByID[id]; !ok {
			block("journal order %q (%s) absent from broker view; history incomplete", j.IntentKey, id)
		}
	}

	lifeBySymbol := make(map[string]tpLifecycle)
	for _, l := range snap.Lifecycles {
		if _, exists := lifeBySymbol[l.Symbol]; exists {
			block("duplicate lifecycle symbol %q", l.Symbol)
		}
		lifeBySymbol[l.Symbol] = l
	}
	posBySymbol := make(map[string]sharedbroker.Object)
	for _, p := range facts.Positions {
		sym := sharedbroker.Text(p, "symbol")
		q, ok := decimalToMicros(sharedbroker.Text(p, "quantity"))
		if sym == "" || !ok {
			block("broker position identity or quantity ambiguous")
			continue
		}
		if _, exists := posBySymbol[sym]; exists {
			block("duplicate broker position %q", sym)
		}
		posBySymbol[sym] = p
		l, ok := lifeBySymbol[sym]
		if !ok {
			block("broker position %q has no TradePulse lifecycle; ownership unproven", sym)
			continue
		}
		lQty, err := lifecycleQtyMicros(l.StateJSON)
		if err != nil {
			block("lifecycle %q quantity unproven: %v", sym, err)
			continue
		}
		if q != lQty {
			block("position %q shares mismatch: broker %d vs lifecycle %d micros (binary roundoff only; microshare changes rejected)", sym, q, lQty)
		}
		var sum int64
		lotsOK := true
		for _, lot := range facts.TaxLots[sym] {
			lq, ok := decimalToMicros(sharedbroker.Text(lot, "quantity"))
			if !ok {
				block("tax lot quantity unproven for %q", sym)
				lotsOK = false
				break
			}
			if lq > q-sum {
				block("tax lot quantity exceeds inventory for %q", sym)
				lotsOK = false
				break
			}
			sum += lq
		}
		if lotsOK && sum != q {
			block("position %q lot sum %d != inventory %d micros", sym, sum, q)
		}
		for _, lot := range facts.TaxLots[sym] {
			lq, ok := decimalToMicros(sharedbroker.Text(lot, "quantity"))
			if !ok || lq == 0 {
				continue
			}
			opener := sharedbroker.Text(lot, "order_id")
			lotID := sharedbroker.Text(lot, "open_lot_id")
			if opener == "" {
				block("tax lot %q in %q has no opener order_id; provenance unproven", lotID, sym)
				continue
			}
			if !brokerUUIDValid(opener) {
				if internalDecimalID(opener) {
					block("tax lot %q opener %q is an internal decimal number, not a broker UUID (production shows 300000... vs broker/journal 6aba... UUIDs); fuzzy amount/date mapping is forbidden", lotID, opener)
				} else {
					block("tax lot %q opener %q is not a broker UUID; ownership unproven", lotID, opener)
				}
				continue
			}
			bo, ok := brokerByID[opener]
			if !ok {
				block("tax lot opener %q in %q is not an observed broker order; manual or unknown provenance blocks", opener, sym)
				continue
			}
			if brokerSymbol(bo) != sym {
				block("tax lot opener %q symbol %q mismatches lot symbol %q", opener, brokerSymbol(bo), sym)
			}
			j, ok := journalByID[opener]
			if !ok {
				block("tax lot opener %q in %q has no journal order; ownership unproven", opener, sym)
				continue
			}
			if strings.ToLower(j.Status) != "filled" {
				block("tax lot opener %q in %q journal status %q is not a filled buy", opener, sym, j.Status)
			}
			if j.Symbol != "" && j.Symbol != sym {
				block("tax lot opener %q journal symbol %q mismatches lot symbol %q", opener, j.Symbol, sym)
			}
			if p, err := payloadFields(j.PayloadJSON); err == nil && p.Side != "" && p.Side != "buy" {
				block("tax lot opener %q journal side %q is not a buy", opener, p.Side)
			}
		}
	}
	for sym, l := range lifeBySymbol {
		if _, ok := posBySymbol[sym]; ok {
			continue
		}
		lQty, err := lifecycleQtyMicros(l.StateJSON)
		if err != nil {
			block("lifecycle %q quantity unproven: %v", sym, err)
		} else if lQty != 0 {
			block("lifecycle %q claims %d micros with no broker position", sym, lQty)
		}
	}
	return blockers
}

func snapshotEvidenceHash(rows []string) string {
	ordered := slices.Clone(rows)
	slices.Sort(ordered)
	h := sha256.New()
	for _, r := range ordered {
		h.Write([]byte(r))
		h.Write([]byte{0})
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func journalEvidenceHash(snap journalSnapshot) string {
	rows := make([]string, 0, len(snap.Orders))
	for _, j := range snap.Orders {
		rows = append(rows, strings.Join([]string{j.IntentKey, j.Symbol, j.PayloadHash, j.Status, j.BrokerOrderID, fmt.Sprint(j.Ambiguous)}, "|"))
	}
	return snapshotEvidenceHash(rows)
}

func lifecycleEvidenceHash(snap journalSnapshot) string {
	rows := make([]string, 0, len(snap.Lifecycles))
	for _, l := range snap.Lifecycles {
		rows = append(rows, strings.Join([]string{l.Symbol, l.StateJSON, l.ReleaseHash, fmt.Sprint(l.UpdatedSequence)}, "|"))
	}
	return snapshotEvidenceHash(rows)
}

func factsDigestValid(facts sharedbroker.Facts) bool {
	if !canonicalHash(facts.Digest) {
		return false
	}
	probe := facts
	probe.Digest = ""
	raw, err := json.Marshal(probe)
	if err != nil {
		return false
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw)) == strings.ToLower(facts.Digest)
}

func readObservedFacts(state string) (sharedbroker.Facts, time.Time, error) {
	var facts sharedbroker.Facts
	f, err := aruntime.PrivateRead(filepath.Join(state, "observed-account.json"))
	if err != nil {
		return facts, time.Time{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return facts, time.Time{}, err
	}
	d := json.NewDecoder(io.LimitReader(f, 16<<20))
	d.UseNumber()
	d.DisallowUnknownFields()
	if err = d.Decode(&facts); err != nil {
		return facts, time.Time{}, errors.New("observed account facts invalid")
	}
	var trailing any
	if d.Decode(&trailing) != io.EOF {
		return facts, time.Time{}, errors.New("observed account facts have trailing JSON")
	}
	return facts, info.ModTime().UTC(), nil
}

func resolveTradePulseDB(tradepulseState string) (string, error) {
	info, err := os.Lstat(tradepulseState)
	if err != nil {
		return "", errors.New("tradepulse state path unavailable")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("tradepulse state path must not be a symlink")
	}
	path := tradepulseState
	if info.IsDir() {
		path = filepath.Join(tradepulseState, "pilot.sqlite3")
	}
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("tradepulse journal database unavailable")
	}
	return path, nil
}

func scanAmbiguous(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case int64:
		return t != 0
	case []byte:
		s := strings.ToLower(string(t))
		return s == "1" || s == "true"
	case string:
		s := strings.ToLower(t)
		return s == "1" || s == "true"
	default:
		return true
	}
}

// readTradePulseJournal opens the peer database read-only and takes one
// consistent read transaction. It never writes.
func readTradePulseJournal(ctx context.Context, dbPath string) (journalSnapshot, []string, error) {
	var snap journalSnapshot
	var structural []string
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=query_only(ON)&_pragma=busy_timeout(5000)")
	if err != nil {
		return snap, nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return snap, nil, err
	}
	defer tx.Rollback()

	intentRows, err := tx.QueryContext(ctx, `SELECT intent_key,symbol,payload_hash,payload_json,status,broker_order_id,ambiguous FROM order_intents`)
	if err != nil {
		structural = append(structural, "tradepulse order_intents unreadable: "+err.Error())
	} else {
		defer intentRows.Close()
		for intentRows.Next() {
			var key, symbol, hash, payload, status string
			var brokerID sql.NullString
			var ambiguous any
			if err = intentRows.Scan(&key, &symbol, &hash, &payload, &status, &brokerID, &ambiguous); err != nil {
				structural = append(structural, "tradepulse order_intents row unreadable: "+err.Error())
				continue
			}
			snap.Orders = append(snap.Orders, tpIntent{
				IntentKey: key, Symbol: symbol, PayloadHash: hash, PayloadJSON: payload,
				Status: status, BrokerOrderID: brokerID.String, Ambiguous: scanAmbiguous(ambiguous),
			})
		}
		if err = intentRows.Err(); err != nil {
			structural = append(structural, "tradepulse order_intents read incomplete: "+err.Error())
		}
		_ = intentRows.Close()
	}
	lifeRows, err := tx.QueryContext(ctx, `SELECT symbol,state_json,release_hash,updated_event_sequence FROM lifecycles`)
	if err != nil {
		structural = append(structural, "tradepulse lifecycles unreadable: "+err.Error())
	} else {
		defer lifeRows.Close()
		for lifeRows.Next() {
			var l tpLifecycle
			if err = lifeRows.Scan(&l.Symbol, &l.StateJSON, &l.ReleaseHash, &l.UpdatedSequence); err != nil {
				structural = append(structural, "tradepulse lifecycles row unreadable: "+err.Error())
				continue
			}
			snap.Lifecycles = append(snap.Lifecycles, l)
		}
		if err = lifeRows.Err(); err != nil {
			structural = append(structural, "tradepulse lifecycles read incomplete: "+err.Error())
		}
		_ = lifeRows.Close()
	}
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0) FROM events`).Scan(&snap.MaxSequence); err != nil {
		structural = append(structural, "tradepulse events sequence unreadable: "+err.Error())
	}
	if err = tx.Commit(); err != nil {
		return snap, structural, err
	}
	return snap, structural, nil
}

// preflight is the read-only ownership observer. It reads fresh broker facts
// and the peer journal, validates exact provenance, and writes a private
// draft-preflight.json receipt. It opens no coordinator ledger, imports
// nothing, and leaves qualified:false and frozen:false in every receipt.
func preflight(ctx context.Context, cfg config, state, tradepulseState string) error {
	now := time.Now().UTC()
	var blockers []string
	block := func(format string, args ...any) {
		blockers = append(blockers, fmt.Sprintf(format, args...))
	}

	var facts sharedbroker.Facts
	var factsAt, fileAt time.Time
	factsOK := false
	if !filepath.IsAbs(tradepulseState) {
		block("tradepulse state path must be absolute")
	}
	observed, modAt, err := readObservedFacts(state)
	if err != nil {
		block("observed account facts unreadable: %v", err)
	} else {
		facts, factsAt, fileAt = observed, observed.At.UTC(), modAt
		factsOK = true
		if age := now.Sub(fileAt); age < -2*time.Second || age > 90*time.Second {
			block("observed account file is stale (age %s); freshness limit 90s", age.Truncate(time.Second))
		}
		if age := now.Sub(factsAt); age < -2*time.Second || age > 90*time.Second {
			block("observed broker facts are stale (age %s); freshness limit 90s", age.Truncate(time.Second))
		}
		want := fmt.Sprintf("%x", sha256.Sum256([]byte(cfg.Account)))
		if facts.AccountHash != want {
			block("observed facts account binding mismatch")
		}
		if !factsDigestValid(facts) {
			block("observed facts digest mismatch")
		}
	}

	var snap journalSnapshot
	if filepath.IsAbs(tradepulseState) {
		dbPath, err := resolveTradePulseDB(tradepulseState)
		if err != nil {
			block("%v", err)
		} else {
			rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			read, structural, err := readTradePulseJournal(rctx, dbPath)
			cancel()
			snap = read
			blockers = append(blockers, structural...)
			if err != nil {
				block("tradepulse journal read failed: %v", err)
			}
		}
	}
	if factsOK {
		blockers = append(blockers, validateProvenance(facts, snap)...)
	} else {
		block("ownership validation skipped without trusted broker facts")
	}

	lots := 0
	for _, rows := range facts.TaxLots {
		lots += len(rows)
	}
	receipt := preflightReceipt{
		At: now, Qualified: false, Frozen: false, EvidenceComplete: len(blockers) == 0,
		FactsDigest: facts.Digest, FactsAt: factsAt,
		BrokerOrders: len(facts.Orders), JournalOrders: len(snap.Orders),
		Positions: len(facts.Positions), Lifecycles: len(snap.Lifecycles),
		TaxLots: lots, MaxSequence: snap.MaxSequence,
		JournalHash: journalEvidenceHash(snap), LifecycleHash: lifecycleEvidenceHash(snap),
		Blockers: append([]string{}, blockers...),
	}
	if receipt.Blockers == nil {
		receipt.Blockers = []string{}
	}
	if err := aruntime.AtomicJSON(filepath.Join(state, "draft-preflight.json"), receipt); err != nil {
		return errors.Join(errors.New("preflight receipt unwritable"), err)
	}
	// Redacted stdout: counts, readiness, blockers and hashes only. No
	// account numbers, token paths or credential material.
	if err := json.NewEncoder(os.Stdout).Encode(receipt); err != nil {
		return err
	}
	if len(blockers) > 0 {
		return fmt.Errorf("ownership preflight blocked (%d): %s", len(blockers), strings.Join(blockers, "; "))
	}
	return nil
}
