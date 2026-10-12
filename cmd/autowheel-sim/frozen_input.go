package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"autowheel/internal/researchselection"
	"autowheel/internal/simulation"
)

// Bind exact development bytes before the price stream is replayed. The
// effective input digest includes them; the CLI also retains the original
// stream digest. No historical frames are changed or filtered.
func bindFrozenInput(source io.Reader, path string) (io.Reader, error) {
	r := bufio.NewReader(source)
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > 1<<20 {
			return nil, errors.New("unbound replay spec exceeds the size limit")
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		break
	}
	var spec simulation.Spec
	d := json.NewDecoder(bytes.NewReader(line))
	d.DisallowUnknownFields()
	if err := d.Decode(&spec); err != nil {
		return nil, err
	}
	var extra any
	if d.Decode(&extra) != io.EOF || spec.ResearchVariant != simulation.ResearchMultiyearROI || spec.Start != "2025-01-02" || spec.End != "2025-12-31" || spec.FrozenDevelopment != nil {
		return nil, errors.New("bind frozen selection only to an unbound full-2025 multiyear input")
	}
	evidence, winner, err := researchselection.Load(path, int64(spec.InitialCash))
	if err != nil {
		return nil, err
	}
	if spec.Config.StrategyPreset != winner && spec.Config.StrategyPreset != researchselection.Baseline {
		return nil, errors.New("held-out replay admits only the frozen 2024 winner and baseline")
	}
	spec.FrozenDevelopment = &evidence
	bound, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	return io.MultiReader(bytes.NewReader(append(bound, '\n')), r), nil
}
