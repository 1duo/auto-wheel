// autowheel-american-audit compares mathematical prices with pinned synthetic
// independent references. It has no credentials, network or broker transport.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"autowheel/internal/americanoption"
)

const dateReferenceSHA = "ce49fb1e742220430fd2cf1237eb5afae000ac7d153512b241dbce5922c0e85c"
const intradayReferenceSHA = "52f69da7c32c3e256aa153464683cc079fbc324b78f697ac9336d8b78e80660c"

// Fixed diagnostics, selected before seeing the Go engine comparisons. These
// are finite-case checks, never global error bounds or historical qualification.
const priceTolerance = .002
const deltaTolerance = .002
const priceGridTolerance = .001
const deltaGridTolerance = .001

type referenceCase struct {
	ID         string              `json:"id"`
	Kind       americanoption.Kind `json:"kind"`
	Status     string              `json:"status"`
	Spot       float64             `json:"spot_synthetic"`
	Strike     float64             `json:"strike_synthetic"`
	Sigma      float64             `json:"sigma_synthetic"`
	Rate       float64             `json:"rate_synthetic"`
	ExpiryDate string              `json:"expiry_iso"`
	Dividends  []dateDividend      `json:"cash_dividends_synthetic"`
	SpotReal   float64             `json:"spot_physical"`
	StrikeReal float64             `json:"strike_physical"`
	SigmaReal  float64             `json:"sigma_physical"`
	RateReal   float64             `json:"rate_physical"`
	NowReal    string              `json:"eval_real_utc"`
	ExpiryReal string              `json:"maturity_real_utc"`
	CashReal   []realDividend      `json:"cash_real_utc"`
	Price      float64             `json:"price_refined_math_usd"`
	Delta      float64             `json:"delta_refined"`
}

type dateDividend struct {
	Amount float64 `json:"amount_math_usd"`
	ExDate string  `json:"ex_date"`
}

type realDividend struct {
	Amount float64 `json:"amount_math_usd"`
	ExAt   string  `json:"real_utc"`
}

type reference struct {
	Synthetic bool            `json:"synthetic"`
	Version   string          `json:"quantlib_actual"`
	Date      string          `json:"evaluation_date_iso"`
	Count     int             `json:"case_count"`
	Failures  int             `json:"failure_count"`
	Cases     []referenceCase `json:"cases"`
}

type comparison struct {
	Family              string                `json:"family"`
	ID                  string                `json:"id"`
	Input               americanoption.Input  `json:"synthetic_input"`
	Standard            *americanoption.Value `json:"standard,omitempty"`
	Refined             *americanoption.Value `json:"refined,omitempty"`
	ReferencePrice      float64               `json:"reference_price_math_usd"`
	ReferenceDelta      float64               `json:"reference_delta"`
	PriceDifference     float64               `json:"price_absolute_difference_math_usd"`
	DeltaDifference     float64               `json:"delta_absolute_difference"`
	PriceGridDifference float64               `json:"price_grid_difference_math_usd"`
	DeltaGridDifference float64               `json:"delta_grid_difference"`
	ElapsedSeconds      float64               `json:"elapsed_seconds_measured"`
	Passed              bool                  `json:"passed"`
	Error               string                `json:"error,omitempty"`
}

type audit struct {
	Schema              string              `json:"schema_version"`
	Model               string              `json:"model"`
	Synthetic           bool                `json:"synthetic"`
	HistoricalQualified bool                `json:"historical_greeks_qualified"`
	ObservedAt          time.Time           `json:"observed_at"`
	ReferenceHashes     map[string]string   `json:"reference_sha256"`
	CodeSHA256          string              `json:"code_sha256"`
	StandardGrid        americanoption.Grid `json:"standard_grid"`
	RefinedGrid         americanoption.Grid `json:"refined_grid"`
	PriceTolerance      float64             `json:"price_tolerance_math_usd"`
	DeltaTolerance      float64             `json:"delta_tolerance"`
	PriceGridTolerance  float64             `json:"price_grid_tolerance_math_usd"`
	DeltaGridTolerance  float64             `json:"delta_grid_tolerance"`
	Cases               []comparison        `json:"cases"`
	Failures            int                 `json:"failure_count"`
	ElapsedSeconds      float64             `json:"elapsed_seconds_measured"`
	Scope               string              `json:"scope"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func loadReference(path, expectedHash string, count int) (reference, error) {
	var result reference
	f, err := os.Open(path)
	if err != nil {
		return result, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 2*1024*1024+1))
	if err != nil {
		return result, err
	}
	h := sha256.Sum256(raw)
	if len(raw) > 2*1024*1024 || hex.EncodeToString(h[:]) != expectedHash {
		return result, errors.New("independent synthetic reference differs from the pinned file")
	}
	if err = json.Unmarshal(raw, &result); err != nil {
		return result, err
	}
	if !result.Synthetic || result.Version != "1.40" || result.Count != count || len(result.Cases) != count || result.Failures != 0 {
		return result, errors.New("reference is not the complete pinned synthetic matrix")
	}
	seen := map[string]bool{}
	for _, c := range result.Cases {
		if c.ID == "" || seen[c.ID] || c.Status != "ok" || !finite(c.Price) || !finite(c.Delta) {
			return result, errors.New("invalid synthetic reference case")
		}
		seen[c.ID] = true
	}
	return result, nil
}

func inputFor(c referenceCase, family, date string) (americanoption.Input, error) {
	in := americanoption.Input{Kind: c.Kind, SpotUSD: c.Spot, StrikeUSD: c.Strike, Sigma: c.Sigma, Rate: c.Rate}
	var err error
	if family == "date_aligned" {
		in.Now, err = time.Parse("2006-01-02", date)
		if err != nil {
			return in, err
		}
		in.Expiry, err = time.Parse("2006-01-02", c.ExpiryDate)
		if err != nil {
			return in, err
		}
		for _, d := range c.Dividends {
			at, err := time.Parse("2006-01-02", d.ExDate)
			if err != nil {
				return in, err
			}
			in.Dividends = append(in.Dividends, americanoption.CashDividend{AmountUSD: d.Amount, ExAt: at})
		}
	} else {
		in.SpotUSD, in.StrikeUSD, in.Sigma, in.Rate = c.SpotReal, c.StrikeReal, c.SigmaReal, c.RateReal
		in.Now, err = time.Parse(time.RFC3339Nano, c.NowReal)
		if err != nil {
			return in, err
		}
		in.Expiry, err = time.Parse(time.RFC3339Nano, c.ExpiryReal)
		if err != nil {
			return in, err
		}
		for _, d := range c.CashReal {
			at, err := time.Parse(time.RFC3339Nano, d.ExAt)
			if err != nil {
				return in, err
			}
			in.Dividends = append(in.Dividends, americanoption.CashDividend{AmountUSD: d.Amount, ExAt: at})
		}
	}
	return in, nil
}

func finite(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }

func run() error {
	root := flag.String("root", ".", "research source checkout")
	datePath := flag.String("date-reference", "research/simulation/2026-10-05/american-reference-20261006.json", "pinned date-aligned reference")
	intradayPath := flag.String("intraday-reference", "research/simulation/2026-10-05/american-intraday-reference-20261006.json", "pinned intraday reference")
	output := flag.String("output", "", "new JSON diagnostic file; refuses overwrite")
	timeSteps := flag.Int("time-steps", 320, "standard time steps; refined uses twice this count")
	spaceSteps := flag.Int("space-steps", 600, "standard space steps; refined uses twice this count")
	flag.Parse()
	if *output == "" || flag.NArg() != 0 || *timeSteps < 256 || *timeSteps > 4096 || *spaceSteps < 256 || *spaceSteps > 4096 {
		return errors.New("use --output NEW_FILE and bounded grid counts")
	}
	date, err := loadReference(*datePath, dateReferenceSHA, 30)
	if err != nil {
		return err
	}
	intraday, err := loadReference(*intradayPath, intradayReferenceSHA, 12)
	if err != nil {
		return err
	}
	code := sha256.New()
	entries, err := os.ReadDir(filepath.Join(*root, "internal/americanoption"))
	if err != nil {
		return err
	}
	var names []string
	for _, entry := range entries { // ReadDir returns names in deterministic order.
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".go") && !strings.HasSuffix(entry.Name(), "_test.go") {
			names = append(names, "internal/americanoption/"+entry.Name())
		}
	}
	names = append(names, "cmd/autowheel-american-audit/main.go")
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(*root, name))
		if err != nil {
			return err
		}
		fmt.Fprintf(code, "%s\x00%d\x00", name, len(raw))
		code.Write(raw)
	}
	result := audit{Schema: "AutoWheelAmericanNumericalAuditV1", Model: americanoption.ModelID, Synthetic: true, ObservedAt: time.Now().UTC(), ReferenceHashes: map[string]string{"date_aligned": dateReferenceSHA, "intraday": intradayReferenceSHA}, CodeSHA256: hex.EncodeToString(code.Sum(nil)), StandardGrid: americanoption.Grid{TimeSteps: *timeSteps, SpaceSteps: *spaceSteps}, RefinedGrid: americanoption.Grid{TimeSteps: *timeSteps * 2, SpaceSteps: *spaceSteps * 2}, PriceTolerance: priceTolerance, DeltaTolerance: deltaTolerance, PriceGridTolerance: priceGridTolerance, DeltaGridTolerance: deltaGridTolerance, Scope: "42 pinned synthetic cases and two Go grids only; diagnostic tolerances are not global error bounds. No IV, causal-source, historical performance, exercise or broker qualification."}
	start := time.Now()
	for _, family := range []struct {
		name string
		ref  reference
	}{{"date_aligned", date}, {"intraday", intraday}} {
		for _, c := range family.ref.Cases {
			row := comparison{Family: family.name, ID: c.ID, ReferencePrice: c.Price, ReferenceDelta: c.Delta}
			row.Input, err = inputFor(c, family.name, family.ref.Date)
			caseStart := time.Now()
			if err == nil {
				var standard, refined americanoption.Value
				standard, err = americanoption.Price(row.Input, result.StandardGrid)
				if err == nil {
					row.Standard = new(standard)
					refined, err = americanoption.Price(row.Input, result.RefinedGrid)
					if err == nil {
						row.Refined = new(refined)
						row.PriceDifference = math.Abs(refined.PriceUSD - c.Price)
						row.DeltaDifference = math.Abs(refined.Delta - c.Delta)
						row.PriceGridDifference = math.Abs(refined.PriceUSD - standard.PriceUSD)
						row.DeltaGridDifference = math.Abs(refined.Delta - standard.Delta)
						row.Passed = row.PriceDifference <= priceTolerance && row.DeltaDifference <= deltaTolerance && row.PriceGridDifference <= priceGridTolerance && row.DeltaGridDifference <= deltaGridTolerance
					}
				}
			}
			if err != nil {
				row.Error = err.Error()
			}
			row.ElapsedSeconds = time.Since(caseStart).Seconds()
			if !row.Passed {
				result.Failures++
			}
			result.Cases = append(result.Cases, row)
		}
	}
	result.ElapsedSeconds = time.Since(start).Seconds()
	raw, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.Write(append(raw, '\n')); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	fmt.Printf("synthetic American audit: %d cases, %d failed diagnostics, %.3f measured seconds\n", len(result.Cases), result.Failures, result.ElapsedSeconds)
	if result.Failures != 0 {
		return errors.New("American numerical diagnostics failed; output preserved")
	}
	return nil
}
