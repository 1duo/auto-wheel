package moomoo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"time"

	"autowheel/internal/contractpolicy"
	"autowheel/internal/wheel"
)

// ExecutionSource is the broker-fact qualification boundary. Implementations
// must derive complete settled funding/ownership from broker evidence, never
// from observation balances or client-provided readiness flags.
type ExecutionSource interface {
	Snapshot(context.Context, wheel.State) (wheel.Snapshot, error)
	Observe(context.Context, string, string, func() time.Time) (wheel.FixedSignalEvidence, error)
	Refresh(context.Context, contractpolicy.Contract, func() time.Time) (wheel.FixedExecutionEvidence, error)
	Review(context.Context, wheel.Candidate) (ExecutionReview, error)
	CloseQuote(context.Context, wheel.State, string) (wheel.Candidate, error)
}

type ExecutionReview struct {
	At                time.Time
	AccountID         string
	CandidateSHA256   string
	Side              string
	FeeCents          wheel.Cents
	PhysicalFeeCents  wheel.Cents
	Deliverable100    bool
	OptionsAuthorized bool
}

// candidatePlacement is the one mapping from an engine candidate to its broker
// limit and duration, shared by submission and recovery.
func candidatePlacement(candidate wheel.Candidate) (wheel.Cents, string) {
	limit, tif := candidate.Bid, "DAY"
	if candidate.Closing {
		limit = candidate.CloseLimit
	}
	if candidate.RestingTakeProfit {
		tif = "GTC"
	}
	return limit, tif
}

func candidateDigest(candidate wheel.Candidate) string {
	raw, err := json.Marshal(candidate)
	if err != nil {
		return ""
	}
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

type Broker struct {
	Config     Config
	Store      *State
	Source     ExecutionSource
	Writer     *TradingClient
	Reader     *Client
	Activation *Activation
	Clock      func() time.Time
	mu         sync.Mutex
	proofs     map[string]ExecutionReview
	snapshotAt time.Time
}

var _ wheel.FixedBroker = (*Broker)(nil)
var _ wheel.FixedCloseBroker = (*Broker)(nil)

func (b *Broker) now() time.Time {
	if b.Clock != nil {
		return b.Clock().UTC()
	}
	return time.Now().UTC()
}

func (b *Broker) configured() error {
	if b == nil || b.Store == nil || b.Source == nil || b.Config.Validate() != nil || b.Config.Strategy.FixedPolicy == nil || b.Config.Strategy.OrderCoveragePolicy != wheel.CompleteMoomooOrders {
		return errors.New("qualified account-bound Moomoo financial policy/source is missing")
	}
	if b.Activation != nil && (b.Activation.Binding.AccountID != b.Config.AccountID || b.Activation.Binding.PolicyID != b.Config.ExecutionPolicy || b.Activation.Binding.ConfigSHA256 != ConfigDigest(b.Config)) {
		return errors.New("Moomoo native configuration differs from activation binding")
	}
	return nil
}

func sourceFresh(at, now time.Time) bool {
	return !at.IsZero() && !at.After(now) && now.Sub(at) <= 30*time.Second
}

func (b *Broker) Snapshot(ctx context.Context) (wheel.Snapshot, error) {
	if err := b.configured(); err != nil {
		return wheel.Snapshot{}, err
	}
	owned, err := b.Store.JournalState()
	if err != nil {
		return wheel.Snapshot{}, err
	}
	if err := b.recoverIntents(ctx, owned); err != nil {
		return wheel.Snapshot{}, err
	}
	if err := b.Store.CheckAttemptFence(); err != nil {
		return wheel.Snapshot{}, err
	}
	snapshot, err := b.Source.Snapshot(ctx, owned)
	if err != nil {
		return wheel.Snapshot{}, err
	}
	if snapshot.Account != b.Config.AccountID || snapshot.OrderCoveragePolicy != wheel.CompleteMoomooOrders || !sourceFresh(snapshot.At, b.now()) || snapshot.AvailableCash < 0 || snapshot.AvailableCash > wheel.MaxMoney {
		return wheel.Snapshot{}, errors.New("Moomoo snapshot account, coverage, settled cash or freshness is unqualified")
	}
	b.mu.Lock()
	b.snapshotAt = snapshot.At
	b.mu.Unlock()
	return snapshot, nil
}

func (b *Broker) RefreshForFixed(ctx context.Context, chosen contractpolicy.Contract, clock func() time.Time) (wheel.FixedExecutionEvidence, error) {
	if err := b.configured(); err != nil {
		return wheel.FixedExecutionEvidence{}, err
	}
	evidence, err := b.Source.Refresh(ctx, chosen, clock)
	if err != nil {
		return wheel.FixedExecutionEvidence{}, err
	}
	if evidence.Market.Contract != chosen || !evidence.Deliverable100 || evidence.ChainID == "" {
		return wheel.FixedExecutionEvidence{}, errors.New("Moomoo refreshed a different or unqualified deliverable")
	}
	return evidence, nil
}

func (b *Broker) CloseQuote(ctx context.Context, state wheel.State, symbol string) (wheel.Candidate, error) {
	if err := b.configured(); err != nil {
		return wheel.Candidate{}, err
	}
	if state.Account != b.Config.AccountID || state.Positions[symbol].PutID == "" && state.Positions[symbol].CallID == "" {
		return wheel.Candidate{}, errors.New("Moomoo close requires an exact owned contract")
	}
	return b.Source.CloseQuote(ctx, state, symbol)
}

func (b *Broker) Review(ctx context.Context, candidate wheel.Candidate) error {
	if err := b.configured(); err != nil {
		return err
	}
	proof, err := b.Source.Review(ctx, candidate)
	if err != nil {
		return err
	}
	sides := []string{"SELL", "SELL_SHORT"}
	if candidate.Closing {
		sides = []string{"BUY", "BUY_BACK"}
	}
	spec := b.Config.Strategy.FixedPolicy.Spec
	if !sourceFresh(proof.At, b.now()) || proof.AccountID != b.Config.AccountID || !validDigest(proof.CandidateSHA256) || proof.CandidateSHA256 != candidateDigest(candidate) || !proof.Deliverable100 || !proof.OptionsAuthorized ||
		proof.FeeCents < 0 || proof.FeeCents > spec.OptionFeeBudget || proof.PhysicalFeeCents < 0 || proof.PhysicalFeeCents > spec.PhysicalFeeBudget || !slices.Contains(sides, proof.Side) {
		return errors.New("Moomoo account review does not prove exact terms, permissions, deliverable and total fee bounds")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.proofs == nil {
		b.proofs = map[string]ExecutionReview{}
	}
	for id, old := range b.proofs {
		if !sourceFresh(old.At, b.now()) {
			delete(b.proofs, id) // A stale review can never authorize a send.
		}
	}
	b.proofs[candidate.ID] = proof
	return nil
}

func (b *Broker) SubmitFixed(ctx context.Context, candidate wheel.Candidate, ref string, deadline time.Time, guard func() error) (string, error) {
	if b.configured() != nil || b.Activation == nil || b.Writer == nil || guard == nil || deadline.IsZero() || ctx.Err() != nil || !validSubmissionRef(ref) {
		return "", wheel.ErrNotSubmitted
	}
	owned, err := b.Store.JournalState()
	if err != nil {
		return "", ErrPlacementOutcomeUnknown
	}
	intent, found := owned.Intents[candidate.Symbol]
	if !found || intent.Ref != ref || intent.OrderID != "" || candidateDigest(intent.Candidate) != candidateDigest(candidate) {
		return "", wheel.ErrNotSubmitted
	}
	b.mu.Lock()
	proof, reviewed := b.proofs[candidate.ID]
	delete(b.proofs, candidate.ID) // Every review is single-use, even if denied.
	b.mu.Unlock()
	if !reviewed || proof.CandidateSHA256 != candidateDigest(candidate) || !sourceFresh(proof.At, b.now()) || b.Writer.accountID != b.Config.AccountID || b.Writer.CheckAccess(ctx) != nil {
		return "", wheel.ErrNotSubmitted
	}
	limit, tif := candidatePlacement(candidate)
	terms := OrderIntentTerms{AccountID: b.Config.AccountID, Ref: ref, Code: candidate.ID, Side: proof.Side, Quantity: 1, LimitCents: limit, TimeInForce: tif, CreatedAt: intent.At}
	if terms.validate(b.now()) != nil || candidate.Quantity != 1 {
		return "", wheel.ErrNotSubmitted
	}
	finalGuard := func() error {
		if b.Activation.CheckReady == nil || b.Activation.CheckReady() != nil {
			return wheel.ErrNotSubmitted
		}
		b.mu.Lock()
		snapshotAt := b.snapshotAt
		b.mu.Unlock()
		now := b.now()
		if ctx.Err() != nil || !now.Before(deadline) || !candidate.TradingWindow(now) || !sourceFresh(snapshotAt, now) || !sourceFresh(proof.At, now) || !sourceFresh(candidate.At, now) || !sourceFresh(candidate.UnderlyingAt, now) {
			return wheel.ErrNotSubmitted
		}
		return guard()
	}
	called, id := false, ""
	err = b.Activation.WithLive(func() error {
		called = true
		if err := finalGuard(); err != nil {
			return wheel.ErrNotSubmitted
		}
		handle, err := b.Store.preparePlacement(terms)
		if err != nil {
			if !b.Store.attemptRecorded(terms.Ref) {
				return wheel.ErrNotSubmitted // No send happens without a durable record.
			}
			return ErrPlacementOutcomeUnknown
		}
		id, err = b.Store.attemptPlacement(ctx, handle, func(context.Context) error { return finalGuard() }, func(ctx context.Context, request PlacementRequest) (int, []byte, error) {
			return b.Writer.sendPlacement(ctx, request, finalGuard)
		})
		return err
	})
	if !called {
		return "", wheel.ErrNotSubmitted
	}
	return id, err
}
