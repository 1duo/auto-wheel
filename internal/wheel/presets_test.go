package wheel

import (
	"slices"
	"testing"
)

func TestMultiyearPresetsAllSymbolsAndRiskBindings(t *testing.T) {
	base := DefaultConfig()
	base.Account, base.Reserve = "12345678", 10000
	for _, id := range StrategyPresetNames() {
		t.Run(id, func(t *testing.T) {
			c, err := base.WithStrategyPreset(id)
			if err != nil || c.Validate() != nil || c.StrategyPreset != id {
				t.Fatalf("invalid preset: %v %+v", err, c)
			}
			if c.Account != base.Account || c.Reserve != base.Reserve || c.MaxCollateral != base.MaxCollateral || c.MaxSymbols != 3 || !slices.Equal(c.Symbols, base.Symbols) {
				t.Fatal("preset changed binding, cash, exposure or universe")
			}
			_, _, _, _, now := fixture()
			for _, symbol := range c.Symbols {
				q := mkPut("candidate", 18000, 150, 160, -.10, now)
				if id == "balanced-roi-v1" {
					q.Delta = -.20
				}
				q.Symbol = symbol
				if _, ok := Select(c, symbol, "put", []Quote{q}, 0, now); !ok {
					t.Fatalf("symbol lacks an eligible strategy: %s", symbol)
				}
			}
			c.PutMaxDelta = .45
			if c.Validate() == nil {
				t.Fatal("modified named preset accepted")
			}
			q := mkPut("modified", 18000, 150, 160, -.10, now)
			if _, ok := Select(c, "NVDA", "put", []Quote{q}, 0, now); ok {
				t.Fatal("selection bypassed preset validation")
			}
		})
	}
}

func TestMultiyearROIRankingAndCostProtection(t *testing.T) {
	_, _, _, _, now := fixture()
	c, _ := DefaultConfig().WithStrategyPreset("conservative-roi-v1")
	low := mkPut("low-delta", 18000, 150, 160, -.06, now)
	high := mkPut("high-roi", 19500, 300, 310, -.14, now)
	for _, quotes := range [][]Quote{{low, high}, {high, low}} {
		if got, ok := Select(c, "NVDA", "put", quotes, 0, now); !ok || got.ID != "high-roi" {
			t.Fatal("ROI preset silently retained delta-first ordering")
		}
	}
	c, _ = c.WithStrategyPreset("cost-protected-roi-v1")
	call := mkCall("below-basis", 20000, 1050, 1060, .80, now)
	if _, ok := Select(c, "NVDA", "call", []Quote{call}, 20500, now); ok {
		t.Fatal("protected call crystallized a loss below share cost")
	}
	call.Strike = 20500
	if _, ok := Select(c, "NVDA", "call", []Quote{call}, 20500, now); !ok {
		t.Fatal("at-cost covered call rejected")
	}
}
