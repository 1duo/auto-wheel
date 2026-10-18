// autowheel-policy-parity compares the production contract kernel with frozen
// historical identities. It reads JSON lines and has no broker or account access.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"autowheel/internal/contractpolicy"
	"autowheel/internal/wheel"
)

type fixture struct {
	Check    string                    `json:"check,omitempty"`
	Mode     string                    `json:"mode"`
	Kind     string                    `json:"kind"`
	Day      string                    `json:"day"`
	Symbol   string                    `json:"symbol"`
	Stock    contractpolicy.Book       `json:"stock"`
	Menu     []contractpolicy.Contract `json:"menu"`
	Expected json.RawMessage           `json:"expected_id"`
	Allowed  *bool                     `json:"expected_allowed"`
	Error    *bool                     `json:"expected_error"`
	Floor    *floorFixture             `json:"floor"`
	Capital  *capitalFixture           `json:"capital"`
	Signal   *string                   `json:"expected_signal"`
}

type floorFixture struct {
	Strike      int64 `json:"strike_cents"`
	Acquisition int64 `json:"acquisition_cents"`
	DeliveryFee int64 `json:"delivery_fee_cents"`
}

type capitalFixture struct {
	Limits contractpolicy.CapitalLimits `json:"limits"`
	Use    contractpolicy.CapitalUse    `json:"use"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	input := bufio.NewScanner(os.Stdin)
	input.Buffer(make([]byte, 64<<10), 8<<20)
	comparisons := 0
	counts := map[string]int{}
	for input.Scan() {
		var f fixture
		d := json.NewDecoder(bytes.NewReader(input.Bytes()))
		d.DisallowUnknownFields()
		if err := d.Decode(&f); err != nil {
			return err
		}
		var extra any
		if d.Decode(&extra) != io.EOF {
			return errors.New("trailing fixture input")
		}
		if err := compare(f); err != nil {
			return err
		}
		counts[f.Check]++
		comparisons++
	}
	if err := input.Err(); err != nil {
		return err
	}
	if comparisons == 0 {
		return errors.New("empty historical parity input")
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"comparisons": comparisons, "mismatches": 0, "financial_replays": 0,
		"checks": counts, "contract_selection_only": len(counts) == 1 && counts[""] > 0, "order_execution_or_returns_qualified": false,
	})
}

func compare(f fixture) error {
	var allowed bool
	var checkErr error
	switch f.Check {
	case "":
		if len(f.Expected) == 0 {
			return errors.New("missing expected identity")
		}
		var expected *string
		if err := json.Unmarshal(f.Expected, &expected); err != nil {
			return err
		}
		selected, err := contractpolicy.Choose(f.Mode, f.Kind, f.Day, f.Symbol, f.Stock, f.Menu)
		if err != nil {
			return err
		}
		if (expected == nil) != (selected == nil) || selected != nil && selected.ID != *expected {
			return fmt.Errorf("contract identity mismatch (%s %s %s %s)", f.Day, f.Symbol, f.Mode, f.Kind)
		}
		return nil
	case "entry_book":
		allowed = contractpolicy.EntryBookUsable(f.Stock)
	case "call_floor":
		if f.Floor == nil || f.Error == nil {
			return errors.New("floor fixture needs bounded costs and expected error state")
		}
		allowed, checkErr = contractpolicy.StockCostCallAllowed(f.Floor.Strike, f.Floor.Acquisition, f.Floor.DeliveryFee)
	case "capital":
		if f.Capital == nil {
			return errors.New("capital fixture missing")
		}
		allowed = contractpolicy.CanOpen(f.Capital.Limits, f.Capital.Use)
	case "calendar":
		if f.Signal == nil {
			return errors.New("calendar fixture needs an expected signal or explicit empty string")
		}
		day, err := time.ParseInLocation(time.DateOnly, f.Day, wheel.Eastern)
		if err != nil {
			return err
		}
		signal, execution, latest, ok := wheel.FixedEntryTimes(day)
		if ok != (*f.Signal != "") || ok && (signal.Format(time.RFC3339) != *f.Signal || execution.Sub(signal) != time.Minute || latest.Sub(signal) != 5*time.Minute) {
			return fmt.Errorf("entry calendar mismatch (%s)", f.Day)
		}
		return nil
	default:
		return errors.New("unknown policy parity check")
	}
	if f.Allowed == nil || allowed != *f.Allowed || (checkErr != nil) != (f.Error != nil && *f.Error) {
		return fmt.Errorf("policy admission mismatch (%s)", f.Check)
	}
	return nil
}
