package main

import (
	"errors"
	"testing"
	"time"

	"autowheel/internal/researchrd1"
)

type fixtureBucketSource struct {
	researchrd1.ReplaySource
	books map[string]researchrd1.Book
}

func (s fixtureBucketSource) Sample(at time.Time) (researchrd1.Observation, error) {
	return researchrd1.Observation{At: at, Options: s.books}, nil
}

type fixtureChoiceSource struct{ fixtureBucketSource }

func (fixtureChoiceSource) Choose(researchrd1.Profile, string, string, string, researchrd1.Book, []researchrd1.Contract) (*researchrd1.Contract, error) {
	return &researchrd1.Contract{ID: "experimental-choice"}, nil
}

// This checks fault isolation/labeling without running an account or history.
func TestMissingHeldMarkFixtureBoundary(t *testing.T) {
	const clock = "2021-01-04T15:59:00-05:00"
	const contractID = "AAPL  210129P00095000"
	scenario, fault, err := replaySelection(missingHeldMarkFixture, clock, contractID)
	if err != nil || fault == nil || scenario != researchrd1.Primary {
		t.Fatalf("fixture does not retain primary mechanics: %s %+v %v", scenario, fault, err)
	}
	original := fixtureBucketSource{books: map[string]researchrd1.Book{contractID: {Bid: 100, Ask: 110}, "other": {Bid: 200, Ask: 210}}}
	source, err := fault.wrap(original)
	if err != nil {
		t.Fatal(err)
	}
	for _, offset := range []time.Duration{0, -time.Minute, time.Minute} {
		observed, err := source.Sample(fault.at.Add(offset))
		_, present := observed.Options[contractID]
		if err != nil || present != (offset != 0) || observed.Options["other"].Ask != 210 || original.books[contractID].Ask != 110 {
			t.Fatalf("fault changed originals or another bucket: offset=%s sample=%+v err=%v", offset, observed, err)
		}
	}
	if _, err := fault.wrap(fixtureBucketSource{books: map[string]researchrd1.Book{}}); err == nil {
		t.Fatal("already missing original accepted as a deliberate omission")
	}
	wrapped, err := fault.wrap(fixtureChoiceSource{fixtureBucketSource: original})
	if err != nil {
		t.Fatal(err)
	}
	chosen, err := wrapped.(missingMarkSource).Choose(researchrd1.Profile{}, "call", "2021-01-04", "AAPL", researchrd1.Book{}, nil)
	if err != nil || chosen == nil || chosen.ID != "experimental-choice" {
		t.Fatal("failure fixture discarded the experiment's selector", chosen, err)
	}
	for _, input := range [][3]string{{"primary", clock, contractID}, {"execution_5m", clock, contractID}, {missingHeldMarkFixture, "", contractID}, {missingHeldMarkFixture, clock, ""}} {
		if _, _, err := replaySelection(input[0], input[1], input[2]); err == nil {
			t.Fatalf("unbound or combined fixture accepted: %v", input)
		}
	}
	expected := errors.New("RD1 held option mark absent")
	result := researchrd1.ReplayResult{Status: "incomplete", Reason: expected.Error(), NetProfit: new(int64(123))}
	if err := fault.finish(&result, expected); !errors.Is(err, expected) || result.NetProfit != nil || string(result.Scenario) != missingHeldMarkFixture || result.Status != "incomplete" {
		t.Fatalf("actual stop lost its label/reason or published profit: %+v %v", result, err)
	}
	result = researchrd1.ReplayResult{Status: "complete", NetProfit: new(int64(123))}
	if err := fault.finish(&result, nil); err == nil || result.Status != "unexpected_fixture_completion" || result.NetProfit != nil {
		t.Fatalf("unexpected success was treated as a valid fixture: %+v %v", result, err)
	}
}
