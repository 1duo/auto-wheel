package researchrd1

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// These are pure parsing/price-threshold checks, not historical account replays.
func TestRD1PacketBoundaries(t *testing.T) {
	t.Run("subcent_expiry_reference", func(t *testing.T) {
		for _, test := range []struct {
			kind   string
			micros int64
			want   string
		}{{"call", 217_009_999, "expired"}, {"call", 217_010_000, "called_away"},
			{"put", 216_990_001, "expired"}, {"put", 216_990_000, "assigned"}} {
			got, err := expiryOutcome(Contract{Kind: test.kind, Strike: 21700}, test.micros)
			if err != nil || got != test.want {
				t.Fatalf("%s reference %d: got %s, %v; want %s", test.kind, test.micros, got, err, test.want)
			}
		}
	})
	t.Run("omitted_fee_is_unknown", func(t *testing.T) {
		raw := `{"dated_fees":{"date":"2021-01-04","known_at":"2020-12-31T00:00:00Z","source_sha256":"` + strings.Repeat("a", 64) + `","option_open_cents":0,"option_close_cents":0,"physical_exercise_cents":0,"stock_sale":{"flat_fee_cents":0,"notional_numerator":0,"notional_denominator":1,"share_numerator":0,"share_denominator":1,"share_cap_cents":0}},"stock_buy_fee_cents":0}`
		raw = strings.TrimSuffix(raw, "}") + `,"physical_stock_sale":{"flat_fee_cents":0,"notional_numerator":0,"notional_denominator":1,"share_numerator":0,"share_denominator":1,"share_cap_cents":0}}`
		var tariff Tariff
		if err := json.Unmarshal([]byte(raw), &tariff); err != nil {
			t.Fatal(err)
		}
		for _, missing := range []string{`"stock_buy_fee_cents":0`, `"option_close_cents":0`, `"physical_exercise_cents":0`} {
			changed := strings.Replace(raw, missing, `"unused":0`, 1)
			if err := json.Unmarshal([]byte(changed), &tariff); err == nil {
				t.Fatalf("omitted fee accepted: %s", missing)
			}
		}
	})
	t.Run("strict_packet_json", func(t *testing.T) {
		for _, raw := range []string{`{"schema":"a","schema":"b"}`, `{"schema":"a","\u0073chema":"b"}`,
			`{"observations":[{"stock_books":{"AAPL":{"bid_cents":1.1}}}]}`, `{"observations":[{"stock_books":{"AAPL":{"bid_cents":1e2}}}]}`, `{} {}`} {
			if _, err := DecodePacket([]byte(raw)); err == nil {
				t.Fatalf("ambiguous/inexact packet accepted: %s", raw)
			}
		}
	})
	t.Run("bucket_identity_is_an_instant", func(t *testing.T) {
		at := time.Date(2021, 1, 4, 20, 29, 0, 0, time.UTC)
		s := &PacketSource{samples: map[time.Time]Observation{at: {At: at, SourceSHA: strings.Repeat("a", 64)}}}
		obs, err := s.Sample(at.In(time.FixedZone("NY-model", -5*60*60)))
		if err != nil || !obs.At.Equal(at) {
			t.Fatal("equivalent RFC3339 offsets lost a source bucket", err)
		}
	})
}
