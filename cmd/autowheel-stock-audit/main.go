// autowheel-stock-audit checks privately retained stock source offline.
// It exports counts, clocks and hashes, never source prices or replay inputs.
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
	"time"

	"autowheel/internal/stockhistory"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "stock audit:", err)
		os.Exit(1)
	}
}

func run() error {
	input := flag.String("input", "", "private MINI MBP-1 JSONL")
	peer := flag.String("peer", "", "optional independently acquired endpoint window")
	cutoff := flag.String("as-of", "", "RFC3339Nano capture cutoff")
	flag.Parse()
	if *input == "" || *cutoff == "" || flag.NArg() != 0 {
		return errors.New("input and as-of are required; no positional arguments")
	}
	asOf, err := time.Parse(time.RFC3339Nano, *cutoff)
	if err != nil {
		return errors.New("invalid as-of")
	}
	series, sourceSHA, err := read(*input)
	if err != nil {
		return err
	}
	if series.Len() == 0 {
		return errors.New("empty source cannot qualify an observation")
	}
	const maxAge = 30 * time.Second
	observations, err := series.Snapshot(asOf, maxAge)
	if err != nil {
		return err
	}
	events := series.Events()
	complete, multiple, tradeOnly, trades, invalidCutoffs := 0, 0, 0, 0, 0
	pending := make(map[string][]stockhistory.Event)
	for _, event := range events {
		if event.TsRecv.After(asOf) {
			continue
		}
		if event.Action == stockhistory.ActionTrade {
			trades++
		}
		pending[event.Symbol] = append(pending[event.Symbol], event)
		if event.Flags&128 == 0 {
			continue
		}
		group := pending[event.Symbol]
		complete++
		if len(group) > 1 {
			multiple++
		}
		allTrades := true
		for _, part := range group {
			allTrades = allTrades && part.Action == stockhistory.ActionTrade
		}
		view, err := series.Snapshot(event.TsRecv, maxAge)
		if err != nil {
			return err
		}
		current, exists := view[event.Symbol]
		if exists && current.Status == stockhistory.StatusInvalid {
			invalidCutoffs++
		}
		if allTrades {
			tradeOnly++
			before, err := series.Snapshot(group[0].TsRecv.Add(-time.Nanosecond), maxAge)
			if err != nil {
				return err
			}
			previous, hadQuote := before[event.Symbol]
			if hadQuote && exists && (current.At != previous.At || current.EventAt != previous.EventAt) {
				return errors.New("trade-only event refreshed a quote clock")
			}
		}
		delete(pending, event.Symbol)
	}
	peerEqual := false
	peerSHA := ""
	if *peer != "" {
		peerSeries, hash, err := read(*peer)
		if err != nil {
			return err
		}
		peerSHA = hash
		peerView, err := peerSeries.Snapshot(asOf, maxAge)
		if err != nil {
			return err
		}
		if len(observations) != len(peerView) {
			return errors.New("endpoint symbol set differs from peer")
		}
		for symbol, current := range observations {
			other, exists := peerView[symbol]
			if !exists || current.At != other.At || current.EventAt != other.EventAt || current.AvailableAt != other.AvailableAt ||
				current.BidPxNano != other.BidPxNano || current.AskPxNano != other.AskPxNano ||
				current.BidSz != other.BidSz || current.AskSz != other.AskSz || current.Status != other.Status {
				return errors.New("endpoint book or original quote clocks differ from peer")
			}
		}
		peerEqual = true
	}
	clocks := make(map[string]map[string]any)
	for symbol, observation := range observations {
		clocks[symbol] = map[string]any{"status": observation.Status, "invalid_reason": observation.InvalidReason,
			"quote_capture": observation.At, "quote_event": observation.EventAt, "available_at": observation.AvailableAt,
			"quote_age_ns": asOf.Sub(observation.At).Nanoseconds(), "trade_capture": observation.TradeAt,
			"trade_event": observation.TradeEventAt, "tie_count": observation.TieCount}
	}
	encoded, err := json.Marshal(observations)
	if err != nil {
		return err
	}
	endpointHash := sha256.Sum256(encoded)
	output := map[string]any{"status": "bounded_source_audit", "source_sha256": sourceSHA,
		"records": series.Len(), "completed_events_at_cutoff": complete, "multi_record_events": multiple,
		"trades": trades, "trade_only_events_checked": tradeOnly, "invalid_completion_cutoffs": invalidCutoffs,
		"unfinished_events_at_cutoff": len(pending), "endpoint_observation_sha256": hex.EncodeToString(endpointHash[:]),
		"observations": clocks, "peer_sha256": peerSHA, "peer_endpoint_equal": peerEqual,
		"source_gate_passed": false, "historical_input_produced": false, "historical_performance_available": false}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(output)
}

func read(path string) (stockhistory.Series, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return stockhistory.Series{}, "", errors.New("cannot open source")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, stockhistory.MaxFileBytes+1))
	if err != nil {
		return stockhistory.Series{}, "", errors.New("cannot read source")
	}
	series, err := stockhistory.ReadMiniMBP(bytes.NewReader(data))
	if err != nil {
		return stockhistory.Series{}, "", err
	}
	hash := sha256.Sum256(data)
	return series, hex.EncodeToString(hash[:]), nil
}
