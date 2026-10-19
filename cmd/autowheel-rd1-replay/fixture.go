package main

import (
	"errors"
	"maps"
	"time"

	"autowheel/internal/researchrd1"
)

const missingHeldMarkFixture = "missing_held_mark_fixture"

// The fault wraps an unchanged admitted packet. It is not a fitted design,
// observed price, or source-admission shortcut.
type missingMarkFault struct {
	at         time.Time
	contractID string
}

func replaySelection(label, clock, contractID string) (researchrd1.Scenario, *missingMarkFault, error) {
	if label != missingHeldMarkFixture {
		if clock != "" || contractID != "" {
			return "", nil, errors.New("RD1 missing-mark flags require the separately charged failure fixture")
		}
		scenario := researchrd1.Scenario(label)
		return scenario, nil, scenario.Validate()
	}
	at, err := time.Parse(time.RFC3339Nano, clock)
	if err != nil || len(contractID) != 21 || at.IsZero() {
		return "", nil, errors.New("RD1 failure fixture requires an exact bucket and held contract identity")
	}
	return researchrd1.Primary, &missingMarkFault{at: at, contractID: contractID}, nil
}

type missingMarkSource struct {
	researchrd1.ReplaySource
	fault missingMarkFault
}

func (s missingMarkSource) Choose(p researchrd1.Profile, kind, day, symbol string, stock researchrd1.Book, menu []researchrd1.Contract) (*researchrd1.Contract, error) {
	if selector, ok := s.ReplaySource.(interface {
		Choose(researchrd1.Profile, string, string, string, researchrd1.Book, []researchrd1.Contract) (*researchrd1.Contract, error)
	}); ok {
		return selector.Choose(p, kind, day, symbol, stock, menu)
	}
	return researchrd1.Choose(p, kind, day, symbol, stock, menu)
}

func (s missingMarkSource) Sample(at time.Time) (researchrd1.Observation, error) {
	observation, err := s.ReplaySource.Sample(at)
	if err == nil && at.Equal(s.fault.at) {
		observation.Options = maps.Clone(observation.Options)
		delete(observation.Options, s.fault.contractID)
	}
	return observation, err
}

func (f missingMarkFault) wrap(source researchrd1.ReplaySource) (researchrd1.ReplaySource, error) {
	observation, err := source.Sample(f.at)
	if err != nil {
		return nil, err
	}
	if _, present := observation.Options[f.contractID]; !present {
		return nil, errors.New("RD1 fixture cannot remove an already absent original mark")
	}
	return missingMarkSource{ReplaySource: source, fault: f}, nil
}

func (f missingMarkFault) finish(result *researchrd1.ReplayResult, replayErr error) error {
	result.Scenario = missingHeldMarkFixture
	result.NetProfit = nil
	if replayErr == nil {
		result.Status = "unexpected_fixture_completion"
		result.Reason = "RD1 missing-held-mark fixture unexpectedly completed"
		return errors.New(result.Reason)
	}
	return replayErr
}
