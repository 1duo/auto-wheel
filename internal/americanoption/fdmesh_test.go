package americanoption

import (
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
	"time"
)

func fdTestBase() Input {
	now := time.Date(2026, time.April, 6, 13, 35, 0, 0, time.UTC)
	return Input{Kind: Call, SpotUSD: 100, StrikeUSD: 100, Sigma: minSigma, Rate: 0.05,
		Now: now, Expiry: now.Add(5 * 24 * time.Hour),
		Dividends: []CashDividend{{AmountUSD: 0.5, ExAt: now.Add(24 * time.Hour)}}}
}

func fdTestSpec() FDMeshSpec {
	return FDMeshSpec{Base: fdTestBase(), Sigmas: []float64{minSigma, 1, maxSigma},
		Primary: Grid{TimeSteps: 256, SpaceSteps: 256}, Diagnostic: Grid{TimeSteps: 256, SpaceSteps: 257},
		PriceToleranceUSD: 1, DeltaTolerance: 1, ImplementationSHA256: strings.Repeat("a", 64)}
}

func stubFD(priceScale func(Grid) float64) func(Input, Grid) (Value, error) {
	return func(in Input, g Grid) (Value, error) {
		off := priceScale(g)
		return Value{PriceUSD: 5 + in.Sigma*2 + off, Delta: 0.5 + in.Sigma*0.01}, nil
	}
}

func TestFDMeshValidation(t *testing.T) {
	price := func(Input, Grid) (Value, error) { t.Fatal("evaluator ran before validation"); return Value{}, nil }
	base := fdTestSpec()
	many := make([]float64, 130)
	for i := range many {
		many[i] = minSigma + float64(i)*(maxSigma-minSigma)/129
	}
	many[len(many)-1] = maxSigma
	cases := map[string]func(*FDMeshSpec){
		"nil mesh":      func(s *FDMeshSpec) { s.Sigmas = nil },
		"single":        func(s *FDMeshSpec) { s.Sigmas = []float64{minSigma} },
		"too many":      func(s *FDMeshSpec) { s.Sigmas = many },
		"missing min":   func(s *FDMeshSpec) { s.Sigmas = []float64{0.5, maxSigma} },
		"missing max":   func(s *FDMeshSpec) { s.Sigmas = []float64{minSigma, 1} },
		"duplicate":     func(s *FDMeshSpec) { s.Sigmas = []float64{minSigma, 1, 1, maxSigma} },
		"decreasing":    func(s *FDMeshSpec) { s.Sigmas = []float64{minSigma, 2, 1, maxSigma} },
		"nan sigma":     func(s *FDMeshSpec) { s.Sigmas = []float64{minSigma, math.NaN(), maxSigma} },
		"upper hash":    func(s *FDMeshSpec) { s.ImplementationSHA256 = strings.Repeat("A", 64) },
		"short hash":    func(s *FDMeshSpec) { s.ImplementationSHA256 = "abc" },
		"nonhex hash":   func(s *FDMeshSpec) { s.ImplementationSHA256 = strings.Repeat("z", 64) },
		"noncanonical":  func(s *FDMeshSpec) { s.Base.Sigma = 0.3 },
		"same grid":     func(s *FDMeshSpec) { s.Diagnostic = s.Primary },
		"neg price tol": func(s *FDMeshSpec) { s.PriceToleranceUSD = -1 },
		"nan delta tol": func(s *FDMeshSpec) { s.DeltaTolerance = math.NaN() },
		"bad spot":      func(s *FDMeshSpec) { s.Base.SpotUSD = 0 },
	}
	for name, mut := range cases {
		s := base
		s.Sigmas = slices.Clone(base.Sigmas)
		s.Base.Dividends = slices.Clone(base.Base.Dividends)
		mut(&s)
		got, ev, err := buildFDMesh(s, price)
		if !errors.Is(err, ErrInvalidInput) || got != nil || ev != nil {
			t.Fatalf("%s: got=%v ev=%v err=%v, want rejected spec before work", name, got, ev, err)
		}
	}
}

func TestFDMeshCloneOwnership(t *testing.T) {
	price := stubFD(func(g Grid) float64 { return float64(g.SpaceSteps) * 1e-3 })
	spec := fdTestSpec()
	built, ev, err := buildFDMesh(spec, price)
	if err != nil || built == nil || ev == nil {
		t.Fatalf("build: %v", err)
	}
	if ev.HistoricalQualified || len(ev.CurveSHA256) != 64 || len(ev.ReportSHA256) != 64 || len(ev.Samples) != 3 {
		t.Fatalf("bad evidence %+v", ev)
	}
	first, err := built.Compatible(MarketPriceInterval{BidCents: 0, AskCents: 3000})
	if err != nil || first.EvidenceSHA256 != ev.ReportSHA256 || first.Set.CurveSHA256 != ev.CurveSHA256 {
		t.Fatalf("compatible binding: %+v %v", first, err)
	}
	spec.Sigmas[1] = 2
	spec.Base.Dividends[0].AmountUSD = 9
	ev.Samples[0].Primary.PriceUSD = 999
	ev.Spec.Sigmas[1] = 2
	again := built.Evidence()
	if again.ReportSHA256 != ev.ReportSHA256 || again.CurveSHA256 != ev.CurveSHA256 || again.Samples[0].Primary.PriceUSD == 999 {
		t.Fatalf("owned report mutated: %+v", again)
	}
	second, err := built.Compatible(MarketPriceInterval{BidCents: 0, AskCents: 3000})
	if err != nil || second.EvidenceSHA256 != first.EvidenceSHA256 || len(second.Set.SigmaIntervals) != len(first.Set.SigmaIntervals) {
		t.Fatalf("compatible changed after caller mutation: %+v vs %+v", first, second)
	}
}

func TestFDMeshFailureNoPartialCurve(t *testing.T) {
	price := func(in Input, g Grid) (Value, error) {
		if in.Sigma == 1 {
			return Value{}, ErrNonConverged
		}
		return Value{PriceUSD: 6, Delta: 0.5}, nil
	}
	built, ev, err := buildFDMesh(fdTestSpec(), price)
	if !errors.Is(err, ErrNonConverged) || built != nil || ev == nil || len(ev.Samples) != 2 || ev.Samples[1].Primary != nil || ev.Samples[1].PrimaryError == "" {
		t.Fatalf("error path must retain the failed attempted node, no curve: built=%v ev=%v err=%v", built, ev, err)
	}
	if _, err := json.Marshal(ev); err != nil {
		t.Fatalf("failure evidence must remain encodable: %v", err)
	}
	price = stubFD(func(g Grid) float64 { return float64(g.SpaceSteps) * 1e-3 })
	strict := fdTestSpec()
	strict.PriceToleranceUSD, strict.DeltaTolerance = 0, 0
	built, ev, err = buildFDMesh(strict, price)
	if !errors.Is(err, ErrNonConverged) || built != nil || ev == nil || len(ev.Samples) == 0 {
		t.Fatalf("zero thresholds must fail without usable curve: built=%v ev=%v err=%v", built, ev, err)
	}
	if _, err := built.Compatible(MarketPriceInterval{}); built != nil || !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("nil curve must not serve Compatible")
	}
}

func TestFDMeshNonfiniteFailureEvidence(t *testing.T) {
	price := func(Input, Grid) (Value, error) { return Value{PriceUSD: math.NaN()}, nil }
	built, ev, err := buildFDMesh(fdTestSpec(), price)
	if built != nil || !errors.Is(err, ErrNonConverged) || ev == nil || len(ev.Samples) != 1 || ev.Samples[0].Primary != nil || ev.Samples[0].Diagnostic != nil {
		t.Fatalf("nonfinite outputs must not become an unbound curve: %+v, %v", ev, err)
	}
	if _, err := json.Marshal(ev); err != nil || len(ev.ReportSHA256) != 64 {
		t.Fatalf("nonfinite failure erased evidence: %v", err)
	}
}
