package researchrd1

import (
	"errors"
	"strings"
	"testing"
	"time"

	"autowheel/internal/researchcampaign"
)

type tariffBoundarySource struct {
	ReplaySource
	err error
}

func (s tariffBoundarySource) Tariff(time.Time) (Tariff, error) {
	return Tariff{}, s.err
}

// This preflight check never constructs financial state or runs an account path.
func TestRD1HaltIsUnfilled(t *testing.T) {
	parse := func(value string) time.Time {
		t.Helper()
		v, err := time.Parse(time.RFC3339, "2021-01-04T"+value+":00-05:00")
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	calendar, err := researchcampaign.NewSessionCalendar([]researchcampaign.ExchangeDay{{
		Date: "2021-01-04", OpenAt: parse("09:30"), CloseAt: parse("16:00"),
		Halts: []researchcampaign.TradingHalt{{From: parse("15:29"), Through: parse("15:31"), EvidenceSHA256: strings.Repeat("a", 64)}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	account := &Account{sessions: calendar}
	tariffReached := errors.New("tariff reached outside halt")
	for _, clock := range []string{"15:28", "15:29", "15:30", "15:31"} {
		when := parse(clock)
		logged := false
		err := executePrimary(account, tariffBoundarySource{err: tariffReached}, Observation{At: when}, Effect{At: when},
			func(_ time.Time, _, stage, reason string, _ *Contract, _ string) error {
				logged = true
				if stage != "unfilled" || reason != "declared_trading_halt" {
					t.Fatalf("wrong halt disposition: %s %s", stage, reason)
				}
				return nil
			})
		if clock == "15:29" || clock == "15:30" {
			if err != nil || !logged {
				t.Fatalf("halt became a failed account path: %s %v", clock, err)
			}
		} else if !errors.Is(err, tariffReached) || logged {
			t.Fatalf("halt leaked outside [from, through): %s %v", clock, err)
		}
	}
}
