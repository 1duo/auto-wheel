package wheel

import (
	"testing"
	"time"
)

func TestFixedPhysicalFeeBoundAndExecution(t *testing.T) {
	for _, tt := range []struct {
		strike int64
		want   Cents
	}{{1, 4}, {200_000, 415}, {54500, 116}} {
		got, ok := fixedPhysicalFeeMinimum(tt.strike)
		if !ok || got != tt.want {
			t.Fatalf("fee at strike %d: %d %v", tt.strike, got, ok)
		}
	}
	for _, strike := range []int64{-1, 0, int64(MaxMoney)/100 + 1, 1 << 62} {
		if _, ok := fixedPhysicalFeeMinimum(strike); ok {
			t.Fatal("invalid notional admitted")
		}
	}
	c, _, _, at, _ := fixedQueueFixture(t)
	e := fixedQueueEvidence("MSFT", "call", at)
	e.Contract.Strike = 200_000
	signal := FixedSignal{Contract: &e.Contract}
	exec := FixedExecutionEvidence{Market: e, ChainID: "chain", Deliverable100: true}
	if _, err := c.executionCandidate(signal, exec, at, true); err == nil {
		t.Fatal("insufficient delivery-fee allowance admitted a call")
	}
	c.spec.PhysicalFeeBudget = 1000
	if _, err := c.executionCandidate(signal, exec, at.Add(time.Second), true); err != nil {
		t.Fatal("reviewed sufficient fee allowance failed", err)
	}
}
