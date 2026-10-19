// autowheel-rd1-replay is an offline, single-account RD1 replay.
// Its external RD1 dispatcher must reserve a charged case before invocation.
// It has no broker client or production configuration/activation interface.
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
	"strings"

	"autowheel/internal/researchcampaign"
	"autowheel/internal/researchrd1"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	packetPath := flag.String("packet", "", "absolute admitted packet path on the RD1 Lexar view")
	packetSHA := flag.String("packet-sha256", "", "exact packet SHA256, bound by the charged reservation")
	output := flag.String("out", "", "new absolute case directory on the same RD1 Lexar view")
	sensitivity := flag.String("sensitivity", "primary", "exact predeclared RD1 scenario; the dispatcher must bind and independently verify it")
	faultAt := flag.String("fixture-missing-held-mark-at", "", "failure fixture only: exact original held-mark bucket")
	faultContract := flag.String("fixture-contract-id", "", "failure fixture only: original held option identity")
	control := flag.String("control", "", "separately charged descriptive control; cash_zero_interest or stock_buy_hold")
	experiment := flag.Bool("experiment", false, "separate reviewed-source experiment with explicit modeled costs and growth-focused calls")
	flag.Parse()
	scenario, fixture, err := replaySelection(*sensitivity, *faultAt, *faultContract)
	if err != nil {
		return err
	}
	if *control != "" && (*control != researchrd1.CashZeroInterest && *control != researchrd1.StockBuyHold || scenario != researchrd1.Primary || fixture != nil) {
		return errors.New("RD1 descriptive control cannot combine a wheel policy, scenario or failure fixture")
	}
	if *experiment && *control != "" {
		return errors.New("experimental packet cannot use an RD1 descriptive control")
	}
	const prefix = "/home/yuduo/Projects/AutoWheel-research/theta-campaign-r6-20261013-1757/reduced-data-rd1/"
	for _, name := range []string{*packetPath, *output} {
		if !filepath.IsAbs(name) || filepath.Clean(name) != name || !strings.HasPrefix(name, prefix) {
			return errors.New("RD1 replay paths must be explicit paths on the private Lexar research view")
		}
	}
	if flag.NArg() != 0 || len(*packetSHA) != 64 || strings.Trim(*packetSHA, "0123456789abcdef") != "" {
		return errors.New("RD1 replay requires one explicit packet hash and no positional arguments")
	}
	info, err := os.Lstat(*packetPath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() <= 0 || info.Size() > researchrd1.MaxPacketBytes {
		return errors.New("RD1 input packet is not bounded, private and regular")
	}
	f, err := os.Open(*packetPath)
	if err != nil {
		return err
	}
	raw, readErr := io.ReadAll(io.LimitReader(f, researchrd1.MaxPacketBytes+1))
	if err := errors.Join(readErr, f.Close()); err != nil {
		return err
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != *packetSHA {
		return errors.New("RD1 input packet differs from charged case hash")
	}
	var replay func(func(researchrd1.Record) error, func(researchrd1.Diagnostic) error) (researchrd1.ReplayResult, error)
	if *control == researchrd1.CashZeroInterest {
		p, err := researchrd1.DecodeCashPacket(raw)
		if err != nil {
			return err
		}
		replay = func(journal func(researchrd1.Record) error, _ func(researchrd1.Diagnostic) error) (researchrd1.ReplayResult, error) {
			return researchrd1.RunCash(p, journal)
		}
	} else if *control == researchrd1.StockBuyHold {
		p, err := researchrd1.DecodeStockPacket(raw)
		if err != nil {
			return err
		}
		replay = func(journal func(researchrd1.Record) error, diagnostics func(researchrd1.Diagnostic) error) (researchrd1.ReplayResult, error) {
			return researchrd1.RunStock(p, journal, diagnostics)
		}
	} else {
		var source researchrd1.ReplaySource
		var sessions *researchcampaign.SessionCalendar
		var clearing *researchcampaign.SettlementCalendar
		var days []researchcampaign.ExchangeDay
		var costs *researchrd1.ExperimentCosts
		var contractAssumption string
		var callEligibility string
		var entryRiskRule string
		if *experiment {
			p, decodeErr := researchrd1.DecodeExperimentPacket(raw)
			if decodeErr != nil {
				return decodeErr
			}
			if p.CallEligibility != "" && fixture != nil {
				return errors.New("experimental call eligibility cannot combine with the legacy mark fixture")
			}
			source, sessions, clearing, days, err = researchrd1.NewExperimentSource(p)
			costs = new(p.Costs)
			contractAssumption = p.ContractAssumption
			callEligibility = p.CallEligibility
			entryRiskRule = p.EntryRiskRule
		} else {
			p, decodeErr := researchrd1.DecodePacket(raw)
			if decodeErr != nil {
				return decodeErr
			}
			source, sessions, clearing, days, err = researchrd1.NewPacketSource(p)
		}
		if err != nil {
			return err
		}
		replaySource := source
		if fixture != nil {
			replaySource, err = fixture.wrap(source)
			if err != nil {
				return err
			}
		}
		replay = func(journal func(researchrd1.Record) error, diagnostics func(researchrd1.Diagnostic) error) (researchrd1.ReplayResult, error) {
			a, err := researchrd1.NewModelAccount(clearing, sessions, journal, scenario.AccountModel())
			if err != nil {
				return researchrd1.ReplayResult{Scenario: scenario, Status: "incomplete", Reason: err.Error()}, err
			}
			result, replayErr := researchrd1.Run(a, replaySource, days, diagnostics, scenario)
			if costs != nil {
				result.ResearchModel = "simple-modeled-costs-growth-calls-v1"
				if callEligibility != "" {
					result.ResearchModel = "simple-modeled-costs-stock-cost-call-floor-v1"
				}
				if entryRiskRule != "" {
					result.ResearchModel += "-entry-minus40-budget-v1"
				}
				result.ModeledCosts = costs
				result.ContractAssumption = contractAssumption
				result.CallEligibility = callEligibility
				result.EntryRiskRule = entryRiskRule
			}
			if fixture != nil {
				replayErr = fixture.finish(&result, replayErr)
			}
			return result, replayErr
		}
	}
	if err := os.Mkdir(*output, 0o700); err != nil {
		return fmt.Errorf("RD1 case output must be new: %w", err)
	}
	open := func(name string) (*os.File, error) {
		return os.OpenFile(filepath.Join(*output, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	}
	journal, err := open("journal.jsonl")
	if err != nil {
		return err
	}
	defer journal.Close()
	diagnostics, err := open("diagnostics.jsonl")
	if err != nil {
		return err
	}
	defer diagnostics.Close()
	appendJSON := func(f *os.File, value any) error {
		if err := json.NewEncoder(f).Encode(value); err != nil {
			return err
		}
		return f.Sync()
	}
	result, replayErr := replay(func(r researchrd1.Record) error { return appendJSON(journal, r) }, func(d researchrd1.Diagnostic) error { return appendJSON(diagnostics, d) })
	resultFile, err := open("result.json")
	if err != nil {
		return err
	}
	if err := errors.Join(appendJSON(resultFile, result), resultFile.Close()); err != nil {
		return err
	}
	if err := errors.Join(journal.Sync(), diagnostics.Sync()); err != nil {
		return err
	}
	directory, err := os.Open(*output)
	if err != nil {
		return err
	}
	if err := errors.Join(directory.Sync(), directory.Close()); err != nil {
		return err
	}
	if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"status": result.Status, "out": *output, "sensitivity": result.Scenario, "independent_financial_verification_complete": false}); err != nil {
		return err
	}
	return replayErr
}
