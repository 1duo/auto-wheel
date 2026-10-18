package robinhood

import (
	"context"
	"errors"
	"time"

	"autowheel/internal/contractpolicy"
	"autowheel/internal/wheel"
)

var _ wheel.FixedBroker = (*Broker)(nil)

func fixedDeadlineAllows(deadline, now time.Time) bool {
	_, execution, latest, known := wheel.FixedEntryTimes(now)
	return known && deadline.Equal(latest) && !now.Before(execution) && !now.After(latest)
}

// RefreshForFixed adds account-specific deliverable proof to the exact-identity
// market refresh. Actual order review and current account funding follow later.
func (b *Broker) RefreshForFixed(ctx context.Context, chosen contractpolicy.Contract, clock func() time.Time) (wheel.FixedExecutionEvidence, error) {
	o, err := b.fixedMarket(ctx, "conservative", chosen.Kind, chosen.Symbol, &chosen, true, clock)
	if err != nil {
		return wheel.FixedExecutionEvidence{}, err
	}
	if o.Choice == nil || *o.Choice != chosen || !o.IdentityRefresh || !o.Structural100 || !o.DeliverableReview || !o.MarketFresh || !o.EntryBookUsable {
		return wheel.FixedExecutionEvidence{}, errors.New("fixed native refresh lacks current exact contract or deliverable proof")
	}
	return wheel.FixedExecutionEvidence{ChainID: o.ChainID, Deliverable100: true,
		Market: wheel.FixedSignalEvidence{Contract: *o.Choice, Stock: o.Stock, Option: o.Option,
			BidAt: o.BidAt, AskAt: o.AskAt, OptionAt: o.OptionAt, Tick: o.Tick, BidSize: o.BidSize}}, nil
}

// SubmitFixed shares the native transport and activation mutex, adding the
// weekly deadline check inside that mutex immediately before broker transport.
// This method does not provision an activation, lock or retired-peer seal.
func (b *Broker) SubmitFixed(ctx context.Context, a wheel.Candidate, ref string, deadline time.Time) (string, error) {
	if b.BeforeSubmit == nil || b.SubmitMu == nil || b.SubmitProvider != nil ||
		b.Config.FixedPolicy == nil || b.Config.OrderCoveragePolicy != "exclusive_standard" || b.Config.CoordinatorSocket != "" || deadline.IsZero() {
		return "", wheel.ErrNotSubmitted
	}
	id, _, err := b.submitEvidence(ctx, a, ref, deadline)
	return id, err
}
