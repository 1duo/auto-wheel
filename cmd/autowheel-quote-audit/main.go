// autowheel-quote-audit compares a receipt-bound control with sparse event
// windows offline. It has no credential, network, broker or financial imports.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	"autowheel/internal/quotehistory"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type comparison struct {
	At           time.Time `json:"at"`
	WindowEvents int       `json:"window_events"`
	Symbols      int       `json:"symbols"`
	QuoteEqual   bool      `json:"quote_state_equal"`
	SeedEvents   int       `json:"seed_events"`
	SeededEqual  bool      `json:"seeded_quote_state_equal"`
}

// Field order and tags match the acquisition client's canonical query identity.
// This local wire shape avoids importing its credential/financial transport.
type controlQuery struct {
	Dataset string   `json:"dataset"`
	Schema  string   `json:"schema"`
	Symbols []string `json:"symbols"`
	StypeIn string   `json:"stype_in"`
	Start   string   `json:"start"`
	End     string   `json:"end"`
	Limit   int      `json:"limit"`
}

type intervalCheck struct {
	Symbol                 string    `json:"symbol"`
	From                   time.Time `json:"from"`
	Through                time.Time `json:"through"`
	SeedPresent            bool      `json:"seed_present"`
	BooksValid             bool      `json:"books_valid"`
	QuotesFresh            bool      `json:"quotes_fresh"`
	EndpointEqual          bool      `json:"endpoint_matches_continuous_snapshot"`
	BookEvents             int       `json:"book_events"`
	TradeEvents            int       `json:"trade_events"`
	MinimaWithinEndpoint   bool      `json:"minima_within_endpoint"`
	LowerBidThanEndpoint   bool      `json:"lower_bid_than_endpoint"`
	LowerSizeThanEndpoint  bool      `json:"lower_size_than_endpoint"`
	InteriorBidDip         bool      `json:"bid_below_both_endpoints"`
	InteriorSizeDip        bool      `json:"size_below_both_endpoints"`
	OriginalStartQuoteTime time.Time `json:"original_start_quote_time,omitzero"`
	OriginalEndQuoteTime   time.Time `json:"original_end_quote_time,omitzero"`
}

func run() error {
	sourcePath := flag.String("source", "", "private downloaded CMBP JSONL")
	receiptPath := flag.String("receipt", "", "saved receipt JSON")
	queryPath := flag.String("query", "", "saved quote/query JSON")
	asOfText := flag.String("as-of", "", "first comparison time, RFC3339Nano")
	output := flag.String("output", "", "new private audit JSON; refuses overwrite")
	flag.Parse()
	if flag.NArg() != 0 || *sourcePath == "" || *receiptPath == "" || *queryPath == "" || *output == "" {
		return errors.New("source, receipt, query, as-of and output are required")
	}
	asOf, err := time.Parse(time.RFC3339Nano, *asOfText)
	if err != nil {
		return errors.New("invalid comparison timestamp")
	}
	raw, err := readBounded(*sourcePath, quotehistory.MaxFileBytes)
	if err != nil {
		return err
	}
	var receipt struct {
		ResponseSHA256 string
		Bytes          int
		OperationID    string
		StatusCode     int
		QueryHash      string
		Endpoint       string
	}
	if err := readJSON(*receiptPath, &receipt); err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	sourceHash := hex.EncodeToString(sum[:])
	if receipt.StatusCode != 200 || receipt.OperationID == "" || receipt.ResponseSHA256 != sourceHash || receipt.Bytes != len(raw) {
		return errors.New("source does not match successful receipt")
	}
	var query struct {
		Query controlQuery
	}
	if err := readJSON(*queryPath, &query); err != nil {
		return err
	}
	q := query.Query
	queryHash, operationID, err := controlIdentity(q)
	if err != nil || receipt.Endpoint != "/timeseries.get_range" || receipt.QueryHash != queryHash || receipt.OperationID != operationID {
		return errors.New("control query does not match receipt identity")
	}
	start, startErr := time.Parse(time.RFC3339Nano, q.Start)
	end, endErr := time.Parse(time.RFC3339Nano, q.End)
	if startErr != nil || endErr != nil || !start.Before(end) || q.Dataset != "OPRA.PILLAR" || q.Schema != "cmbp-1" || q.StypeIn != "raw_symbol" || q.Limit < 1 || q.Limit > quotehistory.MaxEvents || len(q.Symbols) == 0 || asOf.Add(-30*time.Second).Before(start) || !asOf.Add(30*time.Second).Before(end) {
		return errors.New("invalid or insufficient control query envelope")
	}
	events, err := quotehistory.ReadCMBP(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	if len(events) == 0 || len(events) >= q.Limit {
		return errors.New("empty or record-capped control cannot qualify comparison")
	}
	for _, e := range events {
		if e.TsRecv.Before(start) || !e.TsRecv.Before(end) || !slices.Contains(q.Symbols, e.Symbol) {
			return errors.New("source event outside control query")
		}
	}
	points := make([]comparison, 0, 31)
	allEqual := true
	allSeededEqual := true
	for second := range 31 {
		at := asOf.Add(time.Duration(second) * time.Second)
		seed, window := quoteWindow(events, at.Add(-30*time.Second), at)
		full, err := quotehistory.Snapshot(events, at, 30*time.Second)
		if err != nil {
			return err
		}
		sparse, err := quotehistory.Snapshot(window, at, 30*time.Second)
		if err != nil {
			return err
		}
		equal := quoteStatesEqual(full, sparse)
		allEqual = allEqual && equal
		seeded, err := quotehistory.Snapshot(append(seed, window...), at, 30*time.Second)
		if err != nil {
			return err
		}
		seededEqual := quoteStatesEqual(full, seeded)
		allSeededEqual = allSeededEqual && seededEqual
		points = append(points, comparison{at, len(window), len(full), equal, len(seed), seededEqual})
	}
	intervals, err := auditIntervals(events, q.Symbols, asOf)
	if err != nil {
		return err
	}
	invalidChecks, tradeChecks := 0, 0
	for i, e := range events {
		b := e.Book
		invalid := e.Action == quotehistory.ActionReset || b.BidMissing || b.AskMissing || b.BidUndefined || b.AskUndefined || b.BidPxNano <= 0 || b.AskPxNano <= 0 || b.BidSz == 0 || b.AskSz == 0 || b.BidPxNano > b.AskPxNano
		if e.Action != quotehistory.ActionTrade && !invalid {
			continue
		}
		after, err := quotehistory.Snapshot(events[:i+1], e.TsRecv, 30*time.Second)
		if err != nil {
			return err
		}
		if e.Action == quotehistory.ActionTrade {
			before, err := quotehistory.Snapshot(events[:i], e.TsRecv, 30*time.Second)
			if err != nil || !quoteStatesEqual(before, after) {
				return errors.New("trade altered quote state or freshness")
			}
			tradeChecks++
		}
		if e.Action != quotehistory.ActionTrade && invalid {
			if after[e.Symbol].Status != quotehistory.StatusInvalid {
				return errors.New("invalid book event failed to invalidate quote state")
			}
			invalidChecks++
		}
	}
	result := struct {
		Schema                      string          `json:"schema_version"`
		Status                      string          `json:"status"`
		SourceSHA256                string          `json:"source_sha256"`
		OperationID                 string          `json:"operation_id"`
		Records                     int             `json:"records"`
		Bytes                       int             `json:"bytes"`
		SymbolCount                 int             `json:"requested_symbol_count"`
		AllComparedQuoteStatesEqual bool            `json:"all_compared_quote_states_equal"`
		AllSeededQuoteStatesEqual   bool            `json:"all_seeded_quote_states_equal"`
		InvalidStatesVerified       int             `json:"invalid_states_verified"`
		TradeNoRefreshChecks        int             `json:"trade_no_refresh_checks"`
		Comparisons                 []comparison    `json:"comparisons"`
		Intervals                   []intervalCheck `json:"intervals"`
		Limitations                 []string        `json:"limitations"`
	}{
		Schema: "AutoWheelCMBPWindowComparisonV3", Status: "partial_unqualified",
		SourceSHA256: sourceHash, OperationID: receipt.OperationID,
		Records: len(events), Bytes: len(raw), SymbolCount: len(q.Symbols),
		AllComparedQuoteStatesEqual: allEqual, InvalidStatesVerified: invalidChecks,
		AllSeededQuoteStatesEqual: allSeededEqual,
		TradeNoRefreshChecks:      tradeChecks, Comparisons: points,
		Intervals: intervals,
		Limitations: []string{
			"One control query; no full-chain, six-month or venue-loss coverage proof.",
			"Equality excludes last-trade fields, which do not define quote freshness or book state.",
			"Seeded comparison carries each symbol's last original pre-window book event from this control; timestamps are unchanged. This is not a separate sparse acquisition or cost/coverage qualification.",
			"Silence without a prior quote remains absent; do not invent a price or listing conclusion.",
			"No stock/Greek/contract/corporate/lifecycle qualification, selection, fill or performance calculated.",
			"Receipt/query/hash declarations still require independent source review; no source gate passed.",
			"Intervals summarize supplied book events and original age, not transport/venue completeness or standard/tradable attributes. No Complete flag or execution input emitted; raw prices remain private.",
		},
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(append(encoded, '\n'))
	if err := errors.Join(writeErr, f.Sync(), f.Close()); err != nil {
		return err
	}
	fmt.Printf("Audited %d records; %d quote-state comparisons unseeded=%t, seeded=%t; invalid=%d, trades=%d; unqualified for performance.\n", len(events), len(points), allEqual, allSeededEqual, invalidChecks, tradeChecks)
	if !allSeededEqual {
		return errors.New("seeded quote state differs from continuous control; audit saved")
	}
	return nil
}

func controlIdentity(q controlQuery) (queryHash, operationID string, err error) {
	raw, err := json.Marshal(q)
	if err != nil {
		return "", "", errors.New("invalid control query encoding")
	}
	sum := sha256.Sum256(raw)
	h := sha256.New()
	h.Write(raw)
	h.Write([]byte{0})
	h.Write([]byte("/timeseries.get_range"))
	return hex.EncodeToString(sum[:]), hex.EncodeToString(h.Sum(nil)), nil
}

func auditIntervals(events []quotehistory.Event, symbols []string, asOf time.Time) ([]intervalCheck, error) {
	out := make([]intervalCheck, 0, 30*len(symbols))
	for second := range 30 {
		from := asOf.Add(time.Duration(second) * time.Second)
		through := from.Add(time.Second)
		start, err := quotehistory.Snapshot(events, from, 30*time.Second)
		if err != nil {
			return nil, err
		}
		endpoint, err := quotehistory.Snapshot(events, through, 30*time.Second)
		if err != nil {
			return nil, err
		}
		for _, symbol := range symbols {
			v, err := quotehistory.SummarizeInterval(events, symbol, from, through, 30*time.Second)
			if err != nil {
				return nil, err
			}
			book, present := endpoint[symbol]
			initial := start[symbol]
			equal := v.EndPresent == present && (!present || quoteStatesEqual(map[string]quotehistory.Observation{symbol: v.EndObservation}, map[string]quotehistory.Observation{symbol: book}))
			minima := v.MinBidNano >= 0 && v.MinBidSize >= 0 && v.MinBidNano <= book.BidPxNano && v.MinBidSize <= book.BidSz
			if !equal || !minima {
				return nil, errors.New("interval endpoint or minima disagree with continuous snapshot")
			}
			out = append(out, intervalCheck{
				Symbol: symbol, From: from, Through: through, SeedPresent: v.SeedPresent,
				BooksValid: v.BooksValid, QuotesFresh: v.QuotesFresh, EndpointEqual: equal,
				BookEvents: v.BookEvents, TradeEvents: v.TradeEvents, MinimaWithinEndpoint: minima,
				LowerBidThanEndpoint:   v.BooksValid && v.MinBidNano < book.BidPxNano,
				LowerSizeThanEndpoint:  v.BooksValid && v.MinBidSize < book.BidSz,
				InteriorBidDip:         v.BooksValid && v.MinBidNano < initial.BidPxNano && v.MinBidNano < book.BidPxNano,
				InteriorSizeDip:        v.BooksValid && v.MinBidSize < initial.BidSz && v.MinBidSize < book.BidSz,
				OriginalStartQuoteTime: v.StartAt, OriginalEndQuoteTime: v.EndAt,
			})
		}
	}
	return out, nil
}

// quoteWindow retains the last actual book event per symbol before start,
// including stale/invalid/reset state. Seed events remain in original order
// with original timestamps; trades never replace a book or refresh its age.
// Callers must validate the source ordering and completeness separately.
func quoteWindow(events []quotehistory.Event, start, through time.Time) (seed, window []quotehistory.Event) {
	last := make(map[string]int)
	for i, e := range events {
		if e.TsRecv.After(through) {
			break
		}
		if e.TsRecv.Before(start) {
			if e.Action != quotehistory.ActionTrade {
				last[e.Symbol] = i
			}
			continue
		}
		window = append(window, e)
	}
	for i, e := range events {
		if !e.TsRecv.Before(start) {
			break
		}
		if index, ok := last[e.Symbol]; ok && index == i {
			seed = append(seed, e)
		}
	}
	return seed, window
}

func quoteStatesEqual(a, b map[string]quotehistory.Observation) bool {
	if len(a) != len(b) {
		return false
	}
	for symbol, left := range a {
		right, ok := b[symbol]
		left.TradeAt, left.TradeEventAt = time.Time{}, time.Time{}
		right.TradeAt, right.TradeEventAt = time.Time{}, time.Time{}
		if !ok || left != right {
			return false
		}
	}
	return true
}

func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, errors.New("audit input exceeds byte bound or could not be read")
	}
	return raw, nil
}

func readJSON(path string, value any) error {
	raw, err := readBounded(path, 1<<20)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, value)
}
