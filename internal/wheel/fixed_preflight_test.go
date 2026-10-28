package wheel

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestFixedNativePreflightHealthyReadBudget(t *testing.T) {
	c, st, _, config, b, now := fixedExecutionFixture(t)
	config.FixedPolicy.PreflightReads = true
	b.beforeRead = func(n int) {
		want := "ready"
		if n == 2 {
			want = "claimed"
		}
		if st.Fixed.Batch.Signals[0].Phase != want {
			t.Fatal("initial availability read or post-review read crossed its claim boundary")
		}
	}
	if err := c.ExecuteNative(t.Context(), config, b, "MSFT", func() time.Time { return *now }); err != nil || b.readCount != 2 || b.refreshes != 1 || b.reviews != 1 || b.submits != 1 {
		t.Fatal("healthy preflight added broker reads or changed one-shot submission", err)
	}
}

func TestFixedNativePreflightReadRecovery(t *testing.T) {
	for _, scenario := range []string{"snapshot", "quote", "stale"} {
		t.Run(scenario, func(t *testing.T) {
			c, st, store, config, b, now := fixedExecutionFixture(t)
			config.FixedPolicy.PreflightReads = true
			good, cash := b.evidence, b.snapshot.AvailableCash
			switch scenario {
			case "snapshot":
				b.snapshotErr = errors.New("read unavailable")
			case "quote":
				b.refreshErr = errors.New("quote unavailable")
			case "stale":
				b.evidence.Market.AskAt = now.Add(-31 * time.Second)
			}
			if err := c.ExecuteNative(t.Context(), config, b, "MSFT", func() time.Time { return *now }); err == nil {
				t.Fatal("unusable preflight reached execution")
			}
			if st.Fixed.Batch.Signals[0].Phase != "ready" || b.reviews != 0 || b.submits != 0 || len(st.Intents) != 0 {
				t.Fatal("read-only preflight consumed an opportunity or created an order")
			}
			// A new poll after restart may read again, but never reselects identity.
			c, st = restartFixedQueue(t, c, store)
			b.snapshotErr, b.refreshErr = nil, nil
			b.evidence, b.snapshot.AvailableCash = good, cash
			if err := c.ExecuteNative(t.Context(), config, b, "MSFT", func() time.Time { return *now }); err != nil || b.submits != 1 || b.reviews != 1 {
				t.Fatal("healthy preflight did not allow exactly one native attempt", err)
			}
			if st.Intents["MSFT"].Candidate.ID != good.Market.Contract.ID || st.Fixed.Batch.Signals[0].Phase != "claimed" {
				t.Fatal("recovery changed the frozen identity or failed to claim")
			}
			reads := b.readCount
			if err := c.ExecuteNative(t.Context(), config, b, "MSFT", func() time.Time { return *now }); err == nil || b.submits != 1 || b.readCount != reads {
				t.Fatal("consumed opportunity reached new reads or transport")
			}
		})
	}
}

func TestFixedNativePreflightKnownIneligibilityDoesNotStarveRoster(t *testing.T) {
	for _, scenario := range []string{"cash", "deliverable", "liquidity"} {
		t.Run(scenario, func(t *testing.T) {
			c, st, _, config, b, now := fixedExecutionFixture(t)
			config.FixedPolicy.PreflightReads = true
			switch scenario {
			case "cash":
				b.snapshot.AvailableCash = 0
			case "deliverable":
				b.evidence.Deliverable100 = false
			case "liquidity":
				b.evidence.Market.BidSize = 0
			}
			if err := c.ExecuteNative(t.Context(), config, b, "MSFT", func() time.Time { return *now }); err == nil || b.reviews != 0 || b.submits != 0 {
				t.Fatal("known ineligibility reached an order")
			}
			if st.Fixed.Batch.Signals[0].Phase != "claimed" {
				t.Fatal("known ineligibility indefinitely blocked the next roster entry")
			}
			if _, err := c.Claim("AAPL", *now); err != nil {
				t.Fatal("later roster entry remained blocked", err)
			}
		})
	}
}

func TestFixedNativePreflightRetainsPostClaimFences(t *testing.T) {
	for _, scenario := range []string{"unknown", "not_sent", "post_claim_read", "review_changed_cash", "intent_save"} {
		t.Run(scenario, func(t *testing.T) {
			c, st, store, config, b, now := fixedExecutionFixture(t)
			config.FixedPolicy.PreflightReads = true
			switch scenario {
			case "unknown":
				b.submitErr = errors.New("transport outcome unknown")
			case "not_sent":
				b.submitErr = ErrNotSubmitted
			case "post_claim_read":
				b.beforeRead = func(n int) {
					if n == 2 {
						b.snapshotErr = errors.New("read failed after claim")
					}
				}
			case "review_changed_cash":
				b.afterReview = func() { b.snapshot.AvailableCash = 0 }
			case "intent_save":
				c.store = fixedFailIntentStore{store}
			}
			if err := c.ExecuteNative(t.Context(), config, b, "MSFT", func() time.Time { return *now }); err == nil {
				t.Fatal("unsafe execution was accepted")
			}
			wantSubmits := 0
			if scenario == "unknown" || scenario == "not_sent" {
				wantSubmits = 1
			}
			if b.submits != wantSubmits || st.Fixed.Batch.Signals[0].Phase != "claimed" {
				t.Fatal("post-claim failure lost its single-use fence")
			}
			if scenario == "unknown" && len(st.Intents) != 1 {
				t.Fatal("unknown submission lost its durable intent")
			}
			reads := b.readCount
			if c.ExecuteNative(t.Context(), config, b, "MSFT", func() time.Time { return *now }) == nil || b.submits != wantSubmits || b.readCount != reads {
				t.Fatal("post-claim failure automatically retried")
			}
		})
	}
}

func TestFixedNativePreflightDeadlineOrderingAndCancellation(t *testing.T) {
	for _, scenario := range []string{"too_early", "expired", "expires_during_read", "roster_order", "cancelled", "roster_changed"} {
		t.Run(scenario, func(t *testing.T) {
			c, st, _, config, b, now := fixedExecutionFixture(t)
			config.FixedPolicy.PreflightReads = true
			ctx := t.Context()
			symbol := "MSFT"
			switch scenario {
			case "too_early":
				*now = st.Fixed.Batch.ExecutionAt.Add(-time.Nanosecond)
			case "expired":
				*now = st.Fixed.Batch.LatestAt.Add(time.Nanosecond)
			case "expires_during_read":
				b.beforeRead = func(int) { *now = st.Fixed.Batch.LatestAt.Add(time.Nanosecond) }
			case "roster_order":
				symbol = "AAPL"
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "roster_changed":
				config.FixedPolicy.Roster.SourceSHA = fixedDigest("new roster source")
			}
			if err := c.ExecuteNative(ctx, config, b, symbol, func() time.Time { return *now }); err == nil || b.reviews != 0 || b.submits != 0 || len(st.Intents) != 0 {
				t.Fatal("closed, reordered or cancelled preflight reached execution", err)
			}
			if st.Fixed.Batch.Signals[0].Phase != "ready" {
				t.Fatal("preflight outside eligibility consumed a signal")
			}
		})
	}
}
