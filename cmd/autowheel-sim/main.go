// autowheel-sim has no Robinhood transport, credential or service dependency.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"autowheel/internal/simulation"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	input := flag.String("input", "", "JSONL spec followed by ordered historical frames")
	output := flag.String("output", "", "new JSON report file (required; refuses overwrite)")
	root := flag.String("strategy-root", ".", "AutoWheel source checkout for provenance")
	blocker := flag.String("blocker", "", "additional evidenced data-access blocker; suppresses performance")
	fixture := flag.Bool("fixture", false, "run clearly labeled synthetic accounting fixture, never historical performance")
	frozen := flag.String("frozen-selection", "", "frozen 2024 development selection to bind into a 2025 multiyear input")
	flag.Parse()
	if *output == "" || (*fixture && (*input != "" || *frozen != "")) || (!*fixture && *input == "") || flag.NArg() != 0 {
		return errors.New("use --input FILE or --fixture, plus --output NEW_FILE")
	}
	var source io.Reader
	originalInputHash := sha256.New()
	if *fixture {
		source = simulation.AccountingFixture()
	} else {
		f, err := os.Open(*input)
		if err != nil {
			return err
		}
		defer f.Close()
		source = f
		if *frozen != "" {
			source, err = bindFrozenInput(io.TeeReader(source, originalInputHash), *frozen)
			if err != nil {
				return err
			}
		}
	}
	hash := sha256.New()
	for _, name := range []string{"types.go", "strategy.go", "presets.go", "engine.go", "calendar.go"} {
		content, err := os.ReadFile(filepath.Join(*root, "internal", "wheel", name))
		if err != nil {
			return err
		}
		fmt.Fprintf(hash, "%s\x00%d\x00", name, len(content))
		hash.Write(content)
	}
	report := simulation.Replay(source)
	if *frozen != "" {
		report.OriginalInputSHA256 = hex.EncodeToString(originalInputHash.Sum(nil))
	}
	report.StrategySHA256 = hex.EncodeToString(hash.Sum(nil))
	hash.Reset()
	for _, path := range []string{"internal/simulation/types.go", "internal/simulation/replay.go", "internal/simulation/multiyear.go", "internal/researchselection/selection.go", "internal/simulation/selection_omission.go", "internal/simulation/fixture.go", "cmd/autowheel-sim/main.go", "cmd/autowheel-sim/frozen_input.go"} {
		content, err := os.ReadFile(filepath.Join(*root, path))
		if err != nil {
			return err
		}
		fmt.Fprintf(hash, "%s\x00%d\x00", path, len(content))
		hash.Write(content)
	}
	report.EngineSHA256 = hex.EncodeToString(hash.Sum(nil))
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	file, err := os.Open(executable)
	if err != nil {
		return err
	}
	hash.Reset()
	_, err = io.Copy(hash, file)
	if err = errors.Join(err, file.Close()); err != nil {
		return err
	}
	report.ExecutableSHA256 = hex.EncodeToString(hash.Sum(nil))
	if *blocker != "" {
		report.Blockers = append(report.Blockers, *blocker)
		report.Status = "blocked"
		report.Performance = nil
		report.Equity = nil
		report.Trades = nil
		report.EndState = nil
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	fmt.Printf("%s: %s; decisions %d/%d, session valuations %d/%d\n", *output, report.Status, report.ObservedDecisions, report.ExpectedDecisions, report.ObservedValuations, report.ExpectedValuations)
	if report.Status == "blocked" {
		return errors.New("historical performance unavailable: see report blockers")
	}
	return nil
}
