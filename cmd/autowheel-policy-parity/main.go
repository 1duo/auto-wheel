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

	"autowheel/internal/contractpolicy"
)

type fixture struct {
	Mode     string                    `json:"mode"`
	Kind     string                    `json:"kind"`
	Day      string                    `json:"day"`
	Symbol   string                    `json:"symbol"`
	Stock    contractpolicy.Book       `json:"stock"`
	Menu     []contractpolicy.Contract `json:"menu"`
	Expected json.RawMessage           `json:"expected_id"`
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
	for input.Scan() {
		var f fixture
		d := json.NewDecoder(bytes.NewReader(input.Bytes()))
		d.DisallowUnknownFields()
		if err := d.Decode(&f); err != nil {
			return err
		}
		var extra any
		if d.Decode(&extra) != io.EOF || len(f.Expected) == 0 {
			return errors.New("missing expected identity or trailing fixture input")
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
			return fmt.Errorf("contract identity mismatch at fixture %d (%s %s %s %s)", comparisons+1, f.Day, f.Symbol, f.Mode, f.Kind)
		}
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
		"contract_selection_only": true, "order_execution_or_returns_qualified": false,
	})
}
