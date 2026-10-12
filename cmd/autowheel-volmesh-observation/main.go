// autowheel-volmesh-observation evaluates explicitly declared offline models.
// Quotes may come from observations; the model and allowances remain assumptions.
// This command has no broker, network, order, or historical replay dependencies.
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

	"autowheel/internal/americanoption"
)

type observation struct {
	ID          string                             `json:"id"`
	Assumptions string                             `json:"model_assumptions"`
	Spec        americanoption.FDMeshSpec          `json:"spec"`
	Quote       americanoption.MarketPriceInterval `json:"quote"`
}

type request struct {
	Schema string        `json:"schema_version"`
	Cases  []observation `json:"cases"`
}

type caseResult struct {
	ID         string                         `json:"id"`
	Evidence   *americanoption.FDMeshEvidence `json:"evidence,omitempty"`
	Compatible *americanoption.FDMeshResult   `json:"compatible,omitempty"`
	Error      string                         `json:"error,omitempty"`
}

type report struct {
	Schema                    string       `json:"schema_version"`
	InputSHA256               string       `json:"input_sha256"`
	ExecutableSHA256          string       `json:"executable_sha256"`
	ObservedAt                time.Time    `json:"observed_at"`
	Input                     request      `json:"input"`
	Cases                     []caseResult `json:"cases"`
	Failures                  int          `json:"failure_count"`
	HistoricalGreeksQualified bool         `json:"historical_greeks_qualified"`
	HistoricalPerformance     bool         `json:"historical_performance_qualified"`
	Scope                     string       `json:"scope"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func digest(raw []byte) string {
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

func load(path, expected string) (request, error) {
	var input request
	f, err := os.Open(path)
	if err != nil {
		return input, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (2<<20)+1))
	if err != nil {
		return input, err
	}
	if len(raw) > 2<<20 || len(expected) != 64 || digest(raw) != expected {
		return input, errors.New("input exceeds bound or differs from frozen SHA256")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&input); err != nil {
		return input, err
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return input, errors.New("input must contain exactly one JSON value")
	}
	if input.Schema != "AutoWheelFDObservationInputV1" || len(input.Cases) < 1 || len(input.Cases) > 20 {
		return input, errors.New("use AutoWheelFDObservationInputV1 with 1..20 cases")
	}
	seen := make(map[string]bool)
	for _, c := range input.Cases {
		if c.ID == "" || seen[c.ID] || c.Assumptions == "" || c.Spec.ImplementationSHA256 != "" {
			return input, errors.New("cases need distinct IDs, explicit model assumptions and no caller implementation hash")
		}
		seen[c.ID] = true
		for _, g := range []americanoption.Grid{c.Spec.Primary, c.Spec.Diagnostic} {
			if g.TimeSteps < 256 || g.TimeSteps > 2048 || g.SpaceSteps < 256 || g.SpaceSteps > 4096 {
				return input, errors.New("grids must have 256..2048 time and 256..4096 space steps")
			}
		}
	}
	return input, nil
}

func run() error {
	inputPath := flag.String("input", "", "explicit offline model cases")
	inputSHA := flag.String("input-sha256", "", "SHA256 frozen before evaluation")
	output := flag.String("output", "", "new private JSON report; refuses overwrite")
	flag.Parse()
	if *inputPath == "" || *output == "" || flag.NArg() != 0 {
		return errors.New("use --input FILE --input-sha256 HASH --output NEW_FILE")
	}
	input, err := load(*inputPath, *inputSHA)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	f, err := os.Open(executable)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, copyErr := io.Copy(h, f)
	if err := errors.Join(copyErr, f.Close()); err != nil {
		return err
	}
	code := hex.EncodeToString(h.Sum(nil))
	out, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer out.Close()
	result := report{Schema: "AutoWheelFDObservationReportV1", InputSHA256: *inputSHA, ExecutableSHA256: code, Input: input, ObservedAt: time.Now().UTC(), Scope: "Conditional offline American FD curve and compatible volatility/delta intervals. Sampled grid agreement and caller allowances do not prove global numerical error, source causality, dividend absence, historical Greeks, trading eligibility or performance."}
	for _, c := range input.Cases {
		row := caseResult{ID: c.ID}
		spec := c.Spec
		spec.ImplementationSHA256 = code
		mesh, evidence, err := americanoption.BuildFDMesh(spec)
		row.Evidence = evidence
		if err == nil {
			compatible, compatErr := mesh.Compatible(c.Quote)
			err = compatErr
			if err == nil {
				row.Compatible = new(compatible)
			}
		}
		if err != nil {
			row.Error = err.Error()
			result.Failures++
		}
		result.Cases = append(result.Cases, row)
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if result.Failures != 0 {
		return fmt.Errorf("%d conditional model cases failed; full attempted evidence retained", result.Failures)
	}
	return nil
}
