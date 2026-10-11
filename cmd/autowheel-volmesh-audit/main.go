// autowheel-volmesh-audit measures a research curve on pinned synthetic inputs.
// It has no credentials, network, broker transport or historical replay imports.
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

const seedSHA = "30a5c6cba10960a2dfc2c71774583bfb84d6b9d9abac683be4e1e0476b9ca756"

// Declared before measuring this mesh. These are empirical diagnostics, never
// a global PDE or interpolation enclosure or a source-qualification gate.
const gridPriceTolerance = .001
const gridDeltaTolerance = .001
const interpolationPriceTolerance = .01
const interpolationDeltaTolerance = .001

type seed struct {
	Family string               `json:"family"`
	ID     string               `json:"id"`
	Input  americanoption.Input `json:"synthetic_input"`
	Price  float64              `json:"reference_price_math_usd"`
	Delta  float64              `json:"reference_delta"`
}

type midpoint struct {
	Sigma                float64              `json:"sigma"`
	Primary              americanoption.Value `json:"primary"`
	Diagnostic           americanoption.Value `json:"diagnostic"`
	PrimaryPriceError    float64              `json:"primary_interpolation_price_error_math_usd"`
	PrimaryDeltaError    float64              `json:"primary_interpolation_delta_error"`
	DiagnosticPriceError float64              `json:"diagnostic_interpolation_price_error_math_usd"`
	DiagnosticDeltaError float64              `json:"diagnostic_interpolation_delta_error"`
	PriceGridDifference  float64              `json:"price_grid_difference_math_usd"`
	DeltaGridDifference  float64              `json:"delta_grid_difference"`
	Passed               bool                 `json:"passed"`
	Error                string               `json:"error,omitempty"`
}

type caseResult struct {
	Seed                   seed                           `json:"seed"`
	Evidence               *americanoption.FDMeshEvidence `json:"evidence,omitempty"`
	Compatible             *americanoption.FDMeshResult   `json:"synthetic_compatible,omitempty"`
	SeedSigmaEnclosed      bool                           `json:"seed_sigma_enclosed"`
	ReferenceDeltaEnclosed bool                           `json:"reference_delta_enclosed"`
	Midpoints              []midpoint                     `json:"midpoints"`
	Failures               int                            `json:"failure_count"`
	Seconds                float64                        `json:"elapsed_seconds_measured"`
	Error                  string                         `json:"error,omitempty"`
}

type audit struct {
	Schema                      string              `json:"schema_version"`
	Synthetic                   bool                `json:"synthetic"`
	HistoricalQualified         bool                `json:"historical_greeks_qualified"`
	SeedSHA256                  string              `json:"seed_sha256"`
	CodeSHA256                  string              `json:"code_sha256"`
	Primary                     americanoption.Grid `json:"primary_grid"`
	Diagnostic                  americanoption.Grid `json:"diagnostic_grid"`
	Sigmas                      []float64           `json:"sigmas"`
	ObservedAt                  time.Time           `json:"observed_at"`
	GridPriceTolerance          float64             `json:"grid_price_tolerance_math_usd"`
	GridDeltaTolerance          float64             `json:"grid_delta_tolerance"`
	InterpolationPriceTolerance float64             `json:"interpolation_price_tolerance_math_usd"`
	InterpolationDeltaTolerance float64             `json:"interpolation_delta_tolerance"`
	Cases                       []caseResult        `json:"cases"`
	Failures                    int                 `json:"failure_count"`
	Seconds                     float64             `json:"elapsed_seconds_measured"`
	Scope                       string              `json:"scope"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func loadSeeds(path string) ([]seed, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 2<<20+1))
	if err != nil {
		return nil, err
	}
	h := sha256.Sum256(raw)
	if len(raw) > 2<<20 || hex.EncodeToString(h[:]) != seedSHA {
		return nil, errors.New("seed audit differs from pinned successful synthetic inputs")
	}
	var input struct {
		Schema     string `json:"schema_version"`
		Synthetic  bool   `json:"synthetic"`
		Historical bool   `json:"historical_greeks_qualified"`
		Failures   int    `json:"failure_count"`
		Cases      []seed `json:"cases"`
	}
	if err = json.Unmarshal(raw, &input); err != nil {
		return nil, err
	}
	if input.Schema != "AutoWheelAmericanNumericalAuditV1" || !input.Synthetic || input.Historical || input.Failures != 0 || len(input.Cases) != 42 {
		return nil, errors.New("invalid synthetic seed matrix")
	}
	var chosen []seed
	for _, id := range []string{"AM-02", "AM-14", "AM-19"} {
		count := 0
		for _, s := range input.Cases {
			if s.Family == "date_aligned" && s.ID == id {
				chosen = append(chosen, s)
				count++
			}
		}
		if count != 1 {
			return nil, errors.New("synthetic case missing or duplicated")
		}
	}
	return chosen, nil
}

func codeDigest(root string) (string, error) {
	entries, err := os.ReadDir(filepath.Join(root, "internal/americanoption"))
	if err != nil {
		return "", err
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".go") && !strings.HasSuffix(entry.Name(), "_test.go") {
			names = append(names, "internal/americanoption/"+entry.Name())
		}
	}
	names = append(names, "cmd/autowheel-volmesh-audit/main.go")
	h := sha256.New()
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", name, len(raw))
		h.Write(raw)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func run() error {
	root := flag.String("root", ".", "research source checkout")
	seeds := flag.String("seeds", "research/simulation/2026-10-05/american-go-final-20261006.json", "pinned synthetic seed audit")
	output := flag.String("output", "", "new JSON diagnostic; refuses overwrite")
	timeSteps := flag.Int("time-steps", 320, "diagnostic time steps; primary doubles")
	spaceSteps := flag.Int("space-steps", 900, "diagnostic space steps; primary doubles")
	subdivisions := flag.Int("subdivisions", 1, "1..4 subdivisions per original sigma interval")
	flag.Parse()
	if *output == "" || flag.NArg() != 0 || *timeSteps < 256 || *timeSteps > 4096 || *spaceSteps < 256 || *spaceSteps > 4096 || *subdivisions < 1 || *subdivisions > 4 {
		return errors.New("use --output NEW_FILE and bounded grids")
	}
	input, err := loadSeeds(*seeds)
	if err != nil {
		return err
	}
	code, err := codeDigest(*root)
	if err != nil {
		return err
	}
	// Establish exclusive output ownership before the expensive calculation.
	f, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	result := audit{Schema: "AutoWheelFDVolMeshAuditV1", Synthetic: true, SeedSHA256: seedSHA, CodeSHA256: code, ObservedAt: time.Now().UTC(), GridPriceTolerance: gridPriceTolerance, GridDeltaTolerance: gridDeltaTolerance, InterpolationPriceTolerance: interpolationPriceTolerance, InterpolationDeltaTolerance: interpolationDeltaTolerance, Scope: "Three pinned synthetic inputs, complete declared sigma mesh and dual-grid midpoint diagnostics only. Allowances are assumptions; no global PDE/interpolation bound, source, policy, historical performance or live qualification."}
	start := time.Now()
	// Low-volatility carry/contact behavior changes on relative sigma scales;
	// preserve extra low nodes rather than one long .0001..025 segment.
	baseSigmas := []float64{.0001, .00015, .000225, .0003375, .0005, .00075, .001, .0015, .002, .003, .0045, .00675, .01, .015, .025, .05, .075, .1, .15, .2, .25, .3, .4, .5, .6, .75, 1, 1.5, 2, 3, 4, 5}
	sigmas := []float64{baseSigmas[0]}
	for i := range len(baseSigmas) - 1 {
		for j := range *subdivisions - 1 {
			sigmas = append(sigmas, baseSigmas[i]+(baseSigmas[i+1]-baseSigmas[i])*float64(j+1)/float64(*subdivisions))
		}
		sigmas = append(sigmas, baseSigmas[i+1])
	}
	result.Primary = americanoption.Grid{TimeSteps: *timeSteps * 2, SpaceSteps: *spaceSteps * 2}
	result.Diagnostic = americanoption.Grid{TimeSteps: *timeSteps, SpaceSteps: *spaceSteps}
	result.Sigmas = sigmas
	for _, s := range input {
		caseStart := time.Now()
		row := caseResult{Seed: s}
		base := s.Input
		base.Sigma = sigmas[0]
		spec := americanoption.FDMeshSpec{Base: base, Sigmas: sigmas, Primary: americanoption.Grid{TimeSteps: *timeSteps * 2, SpaceSteps: *spaceSteps * 2}, Diagnostic: americanoption.Grid{TimeSteps: *timeSteps, SpaceSteps: *spaceSteps}, PriceToleranceUSD: gridPriceTolerance, DeltaTolerance: gridDeltaTolerance, ImplementationSHA256: code}
		built, ev, buildErr := americanoption.BuildFDMesh(spec)
		row.Evidence = ev
		if buildErr != nil {
			row.Error = buildErr.Error()
			row.Failures++
		} else {
			obs := americanoption.MarketPriceInterval{BidCents: int64(math.Floor(s.Price * 100)), AskCents: int64(math.Ceil(s.Price * 100)), PriceAllowanceUSD: interpolationPriceTolerance, DeltaAllowance: interpolationDeltaTolerance}
			compatible, err := built.Compatible(obs)
			if err != nil {
				row.Error = err.Error()
				row.Failures++
			} else {
				row.Compatible = new(compatible)
				for _, interval := range compatible.Set.SigmaIntervals {
					if interval.Lower <= s.Input.Sigma && s.Input.Sigma <= interval.Upper {
						row.SeedSigmaEnclosed = true
					}
				}
				for _, interval := range compatible.Set.DeltaIntervals {
					if interval.Lower <= s.Delta && s.Delta <= interval.Upper {
						row.ReferenceDeltaEnclosed = true
					}
				}
				if !row.SeedSigmaEnclosed || !row.ReferenceDeltaEnclosed {
					row.Failures++
				}
			}
			for i := range len(ev.Samples) - 1 {
				a, b := ev.Samples[i], ev.Samples[i+1]
				mid := midpoint{Sigma: (a.Sigma + b.Sigma) / 2}
				in := base
				in.Sigma = mid.Sigma
				mid.Primary, err = americanoption.Price(in, spec.Primary)
				if err == nil {
					mid.Diagnostic, err = americanoption.Price(in, spec.Diagnostic)
				}
				if err != nil {
					mid.Error = err.Error()
				} else {
					t := (mid.Sigma - a.Sigma) / (b.Sigma - a.Sigma)
					mid.PrimaryPriceError = math.Abs(mid.Primary.PriceUSD - (a.Primary.PriceUSD + t*(b.Primary.PriceUSD-a.Primary.PriceUSD)))
					mid.PrimaryDeltaError = math.Abs(mid.Primary.Delta - (a.Primary.Delta + t*(b.Primary.Delta-a.Primary.Delta)))
					mid.DiagnosticPriceError = math.Abs(mid.Diagnostic.PriceUSD - (a.Diagnostic.PriceUSD + t*(b.Diagnostic.PriceUSD-a.Diagnostic.PriceUSD)))
					mid.DiagnosticDeltaError = math.Abs(mid.Diagnostic.Delta - (a.Diagnostic.Delta + t*(b.Diagnostic.Delta-a.Diagnostic.Delta)))
					mid.PriceGridDifference = math.Abs(mid.Primary.PriceUSD - mid.Diagnostic.PriceUSD)
					mid.DeltaGridDifference = math.Abs(mid.Primary.Delta - mid.Diagnostic.Delta)
					mid.Passed = mid.PrimaryPriceError <= interpolationPriceTolerance && mid.DiagnosticPriceError <= interpolationPriceTolerance && mid.PrimaryDeltaError <= interpolationDeltaTolerance && mid.DiagnosticDeltaError <= interpolationDeltaTolerance && mid.PriceGridDifference <= gridPriceTolerance && mid.DeltaGridDifference <= gridDeltaTolerance
				}
				if !mid.Passed {
					row.Failures++
				}
				row.Midpoints = append(row.Midpoints, mid)
			}
		}
		row.Seconds = time.Since(caseStart).Seconds()
		result.Failures += row.Failures
		result.Cases = append(result.Cases, row)
		fmt.Printf("volmesh %s: %d retained nodes, %d midpoint checks, %d failed diagnostics, %.3fs\n", s.ID, len(row.EvidenceSamples()), len(row.Midpoints), row.Failures, row.Seconds)
	}
	result.Seconds = time.Since(start).Seconds()
	raw, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	if _, err = f.Write(append(raw, '\n')); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if result.Failures != 0 {
		return errors.New("volmesh diagnostics failed; evidence preserved")
	}
	return nil
}

func (r caseResult) EvidenceSamples() []americanoption.FDMeshSample {
	if r.Evidence == nil {
		return nil
	}
	return r.Evidence.Samples
}
