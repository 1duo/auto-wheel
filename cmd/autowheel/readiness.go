package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"autowheel/internal/robinhood"
	aruntime "autowheel/internal/runtime"
	"autowheel/internal/wheel"
)

type liveReadiness struct {
	At                  time.Time         `json:"at"`
	Ready               bool              `json:"ready"`
	Reason              string            `json:"reason,omitempty"`
	VerifiedSymbols     []string          `json:"verified_symbols,omitempty"`
	Unavailable         map[string]string `json:"unavailable_symbols,omitempty"`
	Preview             *wheel.Candidate  `json:"preview,omitempty"`
	OrderCoveragePolicy string            `json:"order_coverage_policy,omitempty"`
	CoverageWarning     string            `json:"coverage_warning,omitempty"`
}

type activationControl struct {
	Capable     bool
	Config      wheel.Config
	Dir         string
	RetiredPeer *retiredPeer
}

type activationRecord struct {
	ProfitOnly       string    `json:"profit_only,omitempty"`
	Release          string    `json:"release"`
	ExecutableDigest string    `json:"executable_digest"`
	ConfigDigest     string    `json:"config_digest"`
	At               time.Time `json:"at"`
	CoordinatorEpoch int64     `json:"coordinator_epoch,omitzero"`
}

func activationReady(s status) bool {
	if !wheel.CalendarKnown(time.Now()) || !s.LiveCapable || s.LiveReadiness == nil || !s.LiveReadiness.Ready || s.Broker == nil || len(s.Intents) > 0 {
		return false
	}
	if s.Config.OrderCoveragePolicy == "exclusive_standard" && (s.LiveReadiness.OrderCoveragePolicy != "exclusive_standard" || s.Broker.OrderCoveragePolicy != "exclusive_standard" || s.LiveReadiness.CoverageWarning != wheel.StandardOrderWarning) {
		return false
	}
	checkAge, snapshotAge := time.Since(s.LiveReadiness.At), time.Since(s.Broker.At)
	return checkAge >= -2*time.Second && checkAge <= 20*time.Minute && snapshotAge >= -2*time.Second && snapshotAge <= 10*time.Minute
}

func configDigest(c wheel.Config) string {
	raw, _ := json.Marshal(c)
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

func (a *activationControl) enabled() bool {
	if !a.Capable || tradePulseIsolation(a.Config, a.RetiredPeer) != nil {
		return false
	}
	f, err := aruntime.PrivateRead(filepath.Join(a.Dir, "LIVE.json"))
	if err != nil {
		return false
	}
	defer f.Close()
	var r activationRecord
	d := json.NewDecoder(io.LimitReader(f, 4096))
	d.DisallowUnknownFields()
	if d.Decode(&r) != nil {
		return false
	}
	var extra any
	if d.Decode(&extra) != io.EOF || r.Release != version || r.ConfigDigest != configDigest(a.Config) || r.At.IsZero() {
		return false
	}
	digest, err := sharedRelease()
	if err != nil || r.ExecutableDigest != digest {
		return false
	}
	if a.Config.CoordinatorSocket != "" {
		status, err := sharedStatus(a.Config)
		return err == nil && status.Ready && status.Reconciled && status.SingleWriter && status.Reason == "" && status.NewRisk && r.CoordinatorEpoch > 0 && status.Epoch == r.CoordinatorEpoch
	}
	return true
}

func (a *activationControl) enable() error { return a.enablePurpose("") }

func (a *activationControl) enablePurpose(profitID string) error {
	if profitID != "" && a.Config.CoordinatorSocket != "" {
		return errors.New("shared account profit taking unsupported")
	}
	if err := tradePulseIsolation(a.Config, a.RetiredPeer); err != nil {
		return err
	}
	digest, err := sharedRelease()
	if err != nil {
		return errors.New("executable identity cannot be verified")
	}
	epoch := int64(0)
	if a.Config.CoordinatorSocket != "" {
		var err error
		epoch, err = sharedPermission(a.Config, true)
		if err != nil {
			return err
		}
	}
	err = aruntime.AtomicJSON(filepath.Join(a.Dir, "LIVE.json"), activationRecord{ProfitOnly: profitID, Release: version, ExecutableDigest: digest, ConfigDigest: configDigest(a.Config), At: time.Now(), CoordinatorEpoch: epoch})
	if err != nil && a.Config.CoordinatorSocket != "" {
		_, cleanup := sharedPermission(a.Config, false)
		return errors.Join(err, cleanup)
	}
	return err
}

func removeControl(dir, name string) error {
	err := os.Remove(filepath.Join(dir, name))
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err != nil {
		return err
	}
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

// Compatibility reviews cannot authorize a later snapshot without reconciling
// it and persisting any newly confirmed fills or inventory transitions.
func recheckReadiness(c wheel.Config, st *wheel.State, store wheel.Store, s wheel.Snapshot, checked liveReadiness, now time.Time) (liveReadiness, error) {
	if err := wheel.ReconcileForConfig(c, st, s, now); err != nil {
		return liveReadiness{At: now, Reason: err.Error()}, nil
	}
	if err := store.Save(*st); err != nil {
		return liveReadiness{At: now, Reason: "readiness reconciliation could not be persisted"}, err
	}
	if _, err := wheel.Plan(c, *st, s, nil, now); err != nil {
		return liveReadiness{At: now, Reason: err.Error()}, nil
	}
	return checked, nil
}

// Preflight calls only get_* and non-trading review tools. It certifies the
// installed integration, not current tradability: hours/fresh quotes/holdings
// and exact collateral are checked again before every real order.
func preflight(ctx context.Context, c wheel.Config, st wheel.State, b *robinhood.Broker, s wheel.Snapshot, retired ...*retiredPeer) liveReadiness {
	r := liveReadiness{At: time.Now(), Unavailable: map[string]string{}}
	r.OrderCoveragePolicy = c.OrderCoveragePolicy
	if c.OrderCoveragePolicy == "exclusive_standard" {
		r.CoverageWarning = wheel.StandardOrderWarning
	}
	if !wheel.CalendarKnown(r.At) {
		r.Reason = "date is outside the reviewed exchange calendar; live trading blocked"
		return r
	}
	if err := tradePulseIsolation(c, retired...); err != nil {
		r.Reason = err.Error()
		return r
	}
	if _, err := wheel.Plan(c, st, s, nil, time.Now()); err != nil {
		r.Reason = err.Error()
		return r
	}
	if c.SymbolLimitScope != "managed" && len(st.Positions) == 0 {
		active := map[string]bool{}
		for _, symbol := range s.ExternalSymbols {
			active[symbol] = true
		}
		for _, h := range s.Shares {
			if h.Quantity > 0 {
				active[h.Symbol] = true
			}
		}
		for _, p := range s.Options {
			active[p.Symbol] = true
		}
		for _, o := range s.Orders {
			if !o.Terminal() {
				active[o.Symbol] = true
			}
		}
		if len(active) >= c.MaxSymbols {
			r.Reason = "all account-wide symbol slots are occupied by existing holdings"
			return r
		}
	}

	if len(st.Intents) > 0 {
		r.Reason = "unresolved submission"
		return r
	}
	for _, symbol := range c.Symbols {
		if ctx.Err() != nil {
			r.Reason = "preflight timed out"
			return r
		}
		excluded := false
		for _, x := range s.ExternalSymbols {
			if x == symbol {
				excluded = true
			}
		}
		for _, h := range s.Shares {
			if h.Symbol == symbol && st.Positions[symbol].OwnedShares == 0 && st.Positions[symbol].PutID == "" {
				excluded = true
			}
		}
		if excluded {
			r.Unavailable[symbol] = "external holding quarantined"
			continue
		}
		kind := "put"
		if st.Positions[symbol].OwnedShares == 100 {
			kind = "call"
		}
		quotes, err := b.Quotes(ctx, symbol, kind)
		if err != nil {
			r.Unavailable[symbol] = err.Error()
			continue
		}
		if len(quotes) == 0 {
			r.Unavailable[symbol] = "no supported near-term contracts"
			continue
		}
		r.VerifiedSymbols = append(r.VerifiedSymbols, symbol)
		if r.Preview != nil {
			continue
		}
		candidate, ok := wheel.PreviewCandidate(c, symbol, kind, quotes, st.Positions[symbol].Cost, time.Now())
		if !ok {
			r.Unavailable[symbol] = "decoded contracts; no liquid preview candidate"
			continue
		}
		// Preview put collateral only for an empty symbol and within current cash.
		if kind == "put" {
			occupied := st.Positions[symbol].PutID != ""
			for _, p := range s.Options {
				if p.Symbol == symbol {
					occupied = true
				}
			}
			for _, o := range s.Orders {
				if o.Symbol == symbol && !o.Terminal() {
					occupied = true
				}
			}
			if occupied || candidate.Collateral+c.Fee > min(s.AvailableCash-c.Reserve, c.MaxCollateral) {
				continue
			}
		}
		if err = b.PreviewCheck(ctx, candidate); err != nil {
			r.Unavailable[symbol] = err.Error()
			continue
		}
		r.Preview = new(candidate)
	}
	if len(r.VerifiedSymbols) == 0 {
		r.Reason = "no symbol passed actual contract/quote/deliverable checks"
		return r
	}
	if r.Preview == nil {
		r.Reason = "no candidate passed actual fee/collateral review; check market liquidity or account state"
		return r
	}
	r.Ready = true
	return r
}
