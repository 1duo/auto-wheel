// Package americanoption fdmesh is a pure research adapter from accepted
// Price to accepted NewVolMesh. It samples an explicit sigma mesh with two
// declared grids and binds the primary curve with deterministic digests.
// Sampled-grid agreement is a diagnostic only, never global error proof.
// The caller-declared implementation hash is recorded, never a verified
// source-causality claim. Curves are mathematical USD, never ledger money.
package americanoption

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
)

// FDMeshSpec declares the sampled research curve. Base.Sigma must equal
// minSigma (canonical first input); each Sigmas entry overrides it.
type FDMeshSpec struct {
	Base                 Input     `json:"base"`
	Sigmas               []float64 `json:"sigmas"`
	Primary              Grid      `json:"primary"`
	Diagnostic           Grid      `json:"diagnostic"`
	PriceToleranceUSD    float64   `json:"price_tolerance_math_usd"`
	DeltaTolerance       float64   `json:"delta_tolerance"`
	ImplementationSHA256 string    `json:"implementation_sha256"`
}

// FDMeshSample retains full unrounded outputs and absolute differences.
type FDMeshSample struct {
	Sigma           float64 `json:"sigma"`
	Primary         *Value  `json:"primary"`
	Diagnostic      *Value  `json:"diagnostic"`
	PrimaryError    string  `json:"primary_error,omitempty"`
	DiagnosticError string  `json:"diagnostic_error,omitempty"`
	PriceDiffUSD    float64 `json:"price_diff_math_usd"`
	DeltaDiff       float64 `json:"delta_diff"`
}

// FDMeshEvidence is the deterministic diagnostic report. ReportSHA256 hashes
// all fields except itself; it holds no wall-clock or duration measurements.
type FDMeshEvidence struct {
	FDEngine            string         `json:"fd_engine"`
	VolModel            string         `json:"vol_model"`
	Spec                FDMeshSpec     `json:"spec"`
	Samples             []FDMeshSample `json:"samples"`
	CurveSHA256         string         `json:"curve_sha256"`
	ReportSHA256        string         `json:"report_sha256"`
	HistoricalQualified bool           `json:"historical_qualified"`
}

// FDMesh is the immutable built curve over primary-grid values.
type FDMesh struct {
	mesh     *VolMesh
	evidence FDMeshEvidence
}

// FDMeshResult binds a Compatible set to its evidence digest.
type FDMeshResult struct {
	Set            CompatibleVolSet `json:"set"`
	EvidenceSHA256 string           `json:"evidence_sha256"`
}

// BuildFDMesh validates the spec, prices every node on both grids, and on
// success returns the immutable curve plus cloned evidence. Any Price error
// or tolerance breach returns nil curve with diagnostic evidence retaining
// attempted rows and a wrapped ErrInvalidInput/ErrNonConverged.
func BuildFDMesh(spec FDMeshSpec) (*FDMesh, *FDMeshEvidence, error) {
	return buildFDMesh(spec, Price)
}

// Injection is local to a build invocation; production never consults mutable
// package-wide evaluator state.
func buildFDMesh(spec FDMeshSpec, price func(Input, Grid) (Value, error)) (*FDMesh, *FDMeshEvidence, error) {
	owned := FDMeshSpec{
		Base:                 spec.Base,
		Sigmas:               slices.Clone(spec.Sigmas),
		Primary:              spec.Primary,
		Diagnostic:           spec.Diagnostic,
		PriceToleranceUSD:    spec.PriceToleranceUSD,
		DeltaTolerance:       spec.DeltaTolerance,
		ImplementationSHA256: spec.ImplementationSHA256,
	}
	owned.Base.Dividends = slices.Clone(spec.Base.Dividends)
	fail := func(samples []FDMeshSample, err error) (*FDMesh, *FDMeshEvidence, error) {
		if samples == nil {
			return nil, nil, err
		}
		ev := FDMeshEvidence{FDEngine: ModelID, VolModel: VolMeshModelID, Spec: owned, Samples: slices.Clone(samples)}
		digest, hashErr := evidenceDigest(ev)
		if hashErr != nil {
			return nil, nil, errors.Join(err, hashErr)
		}
		ev.ReportSHA256 = digest
		return nil, &ev, err
	}
	if owned.Base.Sigma != minSigma {
		return fail(nil, fmt.Errorf("base sigma must be canonical %v: %w", minSigma, ErrInvalidInput))
	}
	if len(owned.Sigmas) < 2 || len(owned.Sigmas) > 129 {
		return fail(nil, fmt.Errorf("sigma mesh needs 2..129 nodes: %w", ErrInvalidInput))
	}
	for i, s := range owned.Sigmas {
		if !finite(s) || (i > 0 && s <= owned.Sigmas[i-1]) {
			return fail(nil, fmt.Errorf("sigma mesh must be finite strictly increasing: %w", ErrInvalidInput))
		}
	}
	if owned.Sigmas[0] != minSigma || owned.Sigmas[len(owned.Sigmas)-1] != maxSigma {
		return fail(nil, fmt.Errorf("sigma mesh must cover [1e-4,5]: %w", ErrInvalidInput))
	}
	if owned.Primary == owned.Diagnostic {
		return fail(nil, fmt.Errorf("primary and diagnostic grids must be distinct: %w", ErrInvalidInput))
	}
	if !finite(owned.PriceToleranceUSD) || owned.PriceToleranceUSD < 0 || !finite(owned.DeltaTolerance) || owned.DeltaTolerance < 0 {
		return fail(nil, fmt.Errorf("diagnostic thresholds must be finite nonnegative: %w", ErrInvalidInput))
	}
	if !isLowerHex64(owned.ImplementationSHA256) {
		return fail(nil, fmt.Errorf("implementation hash must be lowercase 64 hex: %w", ErrInvalidInput))
	}
	if _, _, _, err := validate(owned.Base, owned.Primary); err != nil {
		return fail(nil, fmt.Errorf("primary grid/input: %w", err))
	}
	if _, _, _, err := validate(owned.Base, owned.Diagnostic); err != nil {
		return fail(nil, fmt.Errorf("diagnostic grid/input: %w", err))
	}
	samples := make([]FDMeshSample, 0, len(owned.Sigmas))
	nodes := make([]VolNode, 0, len(owned.Sigmas))
	for _, sig := range owned.Sigmas {
		in := owned.Base
		in.Sigma = sig
		in.Dividends = owned.Base.Dividends
		p, errP := price(in, owned.Primary)
		d, errD := price(in, owned.Diagnostic)
		row := FDMeshSample{Sigma: sig}
		if errP == nil && (!finite(p.PriceUSD) || !finite(p.Delta) || p.PriceUSD < 0) {
			errP = ErrNonConverged
		}
		if errD == nil && (!finite(d.PriceUSD) || !finite(d.Delta) || d.PriceUSD < 0) {
			errD = ErrNonConverged
		}
		if errP == nil {
			row.Primary = new(p)
		} else {
			row.PrimaryError = errP.Error()
		}
		if errD == nil {
			row.Diagnostic = new(d)
		} else {
			row.DiagnosticError = errD.Error()
		}
		if errP != nil || errD != nil {
			samples = append(samples, row)
			return fail(samples, fmt.Errorf("sigma %v dual-grid price failed: %w", sig, errors.Join(ErrNonConverged, errP, errD)))
		}
		pd, dd := math.Abs(p.PriceUSD-d.PriceUSD), math.Abs(p.Delta-d.Delta)
		if !finite(pd) || !finite(dd) {
			row.PrimaryError = "grid difference cannot be represented"
			samples = append(samples, row)
			return fail(samples, fmt.Errorf("sigma %v non-finite grid output: %w", sig, ErrNonConverged))
		}
		row.PriceDiffUSD, row.DeltaDiff = pd, dd
		if pd > owned.PriceToleranceUSD || dd > owned.DeltaTolerance {
			samples = append(samples, row)
			return fail(samples, fmt.Errorf("sigma %v grid disagreement exceeds tolerance: %w", sig, ErrNonConverged))
		}
		samples = append(samples, row)
		nodes = append(nodes, VolNode{Sigma: sig, PriceUSD: p.PriceUSD, Delta: p.Delta})
	}
	mesh, err := NewVolMesh(nodes)
	if err != nil {
		return fail(samples, fmt.Errorf("primary curve rejected: %w", err))
	}
	ev := FDMeshEvidence{FDEngine: ModelID, VolModel: VolMeshModelID, Spec: owned, Samples: samples, CurveSHA256: mesh.digest}
	ev.ReportSHA256, err = evidenceDigest(ev)
	if err != nil {
		return nil, nil, err
	}
	built := &FDMesh{mesh: mesh, evidence: ev}
	out := cloneEvidence(ev)
	return built, &out, nil
}

// Evidence returns a deep clone; mutating it cannot affect the owned curve.
func (m *FDMesh) Evidence() FDMeshEvidence {
	if m == nil {
		return FDMeshEvidence{FDEngine: ModelID, VolModel: VolMeshModelID}
	}
	return cloneEvidence(m.evidence)
}

// Compatible calls the underlying primary-value VolMesh and binds the result
// to the evidence digest. It never invents a point IV or delta.
func (m *FDMesh) Compatible(obs MarketPriceInterval) (FDMeshResult, error) {
	if m == nil || m.mesh == nil {
		return FDMeshResult{}, fmt.Errorf("no usable curve: %w", ErrInvalidInput)
	}
	set, err := m.mesh.Compatible(obs)
	if err != nil {
		return FDMeshResult{}, err
	}
	return FDMeshResult{Set: set, EvidenceSHA256: m.evidence.ReportSHA256}, nil
}

func isLowerHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func evidenceDigest(ev FDMeshEvidence) (string, error) {
	raw, err := json.Marshal(struct {
		FDEngine string         `json:"fd_engine"`
		VolModel string         `json:"vol_model"`
		Spec     FDMeshSpec     `json:"spec"`
		Samples  []FDMeshSample `json:"samples"`
		CurveSHA string         `json:"curve_sha256"`
		HistQual bool           `json:"historical_qualified"`
	}{ev.FDEngine, ev.VolModel, ev.Spec, ev.Samples, ev.CurveSHA256, ev.HistoricalQualified})
	if err != nil {
		return "", fmt.Errorf("mesh evidence cannot be encoded: %w", errors.Join(ErrInvalidInput, err))
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func cloneEvidence(ev FDMeshEvidence) FDMeshEvidence {
	out := ev
	out.Spec.Sigmas = slices.Clone(ev.Spec.Sigmas)
	out.Spec.Base.Dividends = slices.Clone(ev.Spec.Base.Dividends)
	out.Samples = slices.Clone(ev.Samples)
	for i, row := range out.Samples {
		if row.Primary != nil {
			out.Samples[i].Primary = new(*row.Primary)
		}
		if row.Diagnostic != nil {
			out.Samples[i].Diagnostic = new(*row.Diagnostic)
		}
	}
	return out
}
