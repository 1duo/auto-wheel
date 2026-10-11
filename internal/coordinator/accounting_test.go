package coordinator

import (
	"encoding/json"
	"testing"
	"time"
)

func accepted(t *testing.T, l *Ledger, r Request) Intent {
	t.Helper()
	in := prepare(t, l, "tradepulse", r)
	if _, err := l.StartSend("tradepulse", r.IntentKey, in.TicketVersion, r.Digest, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := l.Complete("tradepulse", r.IntentKey, r.IntentKey, json.RawMessage(`{"receipt":true}`)); err != nil {
		t.Fatal(err)
	}
	return in
}
func TestTerminalCapitalCannotBeUnderstated(t *testing.T) {
	l, _ := seed(t, 1_000_000)
	r := req("buy", "AAA", "stock_buy", 100_000)
	accepted(t, l, r)
	o := Ownership{UnderlyingID: "AAA", Symbol: "AAA", Strategy: "tradepulse", QuantityMicros: 1_000_000, CostCents: 1}
	p := TerminalProof{BrokerOrderID: "buy", EvidenceDigest: proof, FilledMicros: 1_000_000, DebitCents: 100_000, Ownership: o}
	if err := l.ApplyTerminal("tradepulse", "buy", p); err == nil {
		t.Fatal("buy understated committed cost")
	}
	p.Ownership.CostCents = 100_000
	if err := l.ApplyTerminal("tradepulse", "buy", p); err != nil {
		t.Fatal(err)
	}
	sell := req("sell", "AAA", "stock_sell", 100)
	sell.QuantityMicros = 100_000
	sell.SharesMicros = 100_000
	accepted(t, l, sell)
	st, _ := l.Status()
	o = st.Owners[0]
	o.QuantityMicros = 900_000
	o.CostCents = 0
	p = TerminalProof{BrokerOrderID: "sell", EvidenceDigest: proof, FilledMicros: 100_000, DebitCents: 100, Ownership: o}
	if err := l.ApplyTerminal("tradepulse", "sell", p); err == nil {
		t.Fatal("partial sale erased remaining committed cost")
	}
	p.Ownership.CostCents = 90_000
	if err := l.ApplyTerminal("tradepulse", "sell", p); err != nil {
		t.Fatal(err)
	}
	st, _ = l.Status()
	if st.CapitalCents != 90_000 || st.FreeCashCents != 899_900 {
		t.Fatal("partial cancellation accounting wrong", st)
	}
	if err := l.ApplyTerminal("tradepulse", "sell", p); err != nil {
		t.Fatal("exact terminal repeat failed", err)
	}
}
func TestRemainingCapitalRoundsConservativelyWithoutOverflow(t *testing.T) {
	for _, tc := range []struct{ cost, prior, remaining, want int64 }{{1, 3, 1, 1}, {8_000_000, 100_000_000_000_000, 99_999_999_999_999, 8_000_000}, {300, 3, 0, 0}} {
		got, err := RemainingCost(tc.cost, tc.prior, tc.remaining)
		if err != nil || got != tc.want {
			t.Fatal(tc, got, err)
		}
	}
}
func TestBlockingInvalidatesAllTickets(t *testing.T) {
	l, _ := seed(t, 1_000_000)
	in := prepare(t, l, "tradepulse", req("pending", "AAA", "stock_buy", 100_000))
	if err := l.SetBlocked("unknown settlement"); err != nil {
		t.Fatal(err)
	}
	if err := l.SetBlocked(""); err != nil {
		t.Fatal(err)
	}
	if _, err := l.StartSend("tradepulse", "pending", in.TicketVersion, in.Request.Digest, time.Now()); err == nil {
		t.Fatal("old ticket revived after block cleared")
	}
}
