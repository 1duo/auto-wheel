package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"autowheel/internal/robinhood"
	aruntime "autowheel/internal/runtime"
	"autowheel/internal/wheel"
)

type profitPreview struct {
	ID            string          `json:"id"`
	ExpiresAt     time.Time       `json:"expires_at"`
	Candidate     wheel.Candidate `json:"candidate"`
	ClosingFee    wheel.Cents     `json:"closing_fee_cents"`
	FeeAllowance  wheel.Cents     `json:"fee_allowance_cents"`
	MinimumProfit wheel.Cents     `json:"minimum_profit_cents"`
}

type profitControl struct {
	activation        *activationControl
	config            wheel.Config
	token             string
	state             *wheel.State
	store             wheel.Store
	cycleMu, submitMu *sync.Mutex
	read              func() status
	wake              chan<- struct{}
	busy              atomic.Bool
	preview           *profitPreview // Protected by cycleMu; deliberately not replayed after restart.
}

// Any unreadable record is treated as restricted. enabled separately validates
// the complete release/config/executable binding before a submission is allowed.
func (a *activationControl) profitOnly() string {
	f, err := aruntime.PrivateRead(filepath.Join(a.Dir, "LIVE.json"))
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		return "invalid"
	}
	defer f.Close()
	var r activationRecord
	if json.NewDecoder(io.LimitReader(f, 4096)).Decode(&r) != nil {
		return "invalid"
	}
	return r.ProfitOnly
}

func (p *profitControl) register(mux *http.ServeMux, dir string) {
	for _, action := range []string{"profit-preview", "take-profit"} {
		mux.HandleFunc("POST /control/"+action+"/{value}", func(w http.ResponseWriter, r *http.Request) {
			if !nativeControlRequest(r, dir) {
				http.Error(w, "native operator request required", http.StatusForbidden)
				return
			}
			if !p.cycleMu.TryLock() {
				http.Error(w, "Broker reconciliation in progress; refresh before requesting a preview", http.StatusConflict)
				return
			}
			defer p.cycleMu.Unlock()
			ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			defer cancel()
			w.Header().Set("Cache-Control", "no-store")
			if action == "profit-preview" {
				preview, err := p.prepare(ctx, r.PathValue("value"))
				if err != nil {
					http.Error(w, err.Error(), http.StatusConflict)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				slog.Info("operator profit preview", "symbol", preview.Candidate.Symbol, "limit_cents", preview.Candidate.CloseLimit)
				json.NewEncoder(w).Encode(preview)
				return
			}
			if err := p.execute(ctx, r.PathValue("value")); err != nil {
				slog.Warn("operator profit action ended", "reason", err.Error())
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			slog.Info("operator profit close submitted; entries paused")
			json.NewEncoder(w).Encode(map[string]string{"status": "submitted; paused; awaiting broker reconciliation"})
		})
	}
}

func (p *profitControl) broker(ctx context.Context, live bool) (*robinhood.Broker, error) {
	if p.config.CoordinatorSocket != "" || p.config.OrderCoveragePolicy != "exclusive_standard" {
		return nil, errors.New("profit taking requires the exclusive account deployment")
	}
	if err := tradePulseIsolation(p.config, p.activation.RetiredPeer); err != nil {
		return nil, err
	}
	client := robinhood.NewClient(p.token, live)
	if err := client.Connect(ctx); err != nil {
		return nil, err
	}
	return &robinhood.Broker{MCP: client, Config: p.config, SubmitMu: p.submitMu}, nil
}

func (p *profitControl) prepare(ctx context.Context, symbol string) (*profitPreview, error) {
	p.preview = nil
	b, err := p.broker(ctx, false)
	if err != nil {
		return nil, err
	}
	s, err := b.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	if err = wheel.ReconcileForConfig(p.config, p.state, s, time.Now()); err != nil {
		return nil, err
	}
	if err = p.store.Save(*p.state); err != nil {
		return nil, err
	}
	a, err := b.CloseQuote(ctx, *p.state, symbol)
	if err != nil {
		return nil, err
	}
	if err = wheel.CheckClose(p.config, *p.state, s, a, time.Now()); err != nil {
		return nil, err
	}
	review, err := b.ReviewEvidence(ctx, a)
	if err != nil {
		return nil, err
	}
	// ReviewEvidence has validated the presence, exact decimal and fee ceiling.
	fees := review["fees"].(map[string]any)
	feeText, ok := fees["total_fee"].(string)
	if !ok {
		return nil, errors.New("closing fee is not an exact decimal")
	}
	fee, err := wheel.ParseCents(feeText)
	if err != nil {
		return nil, err
	}
	p.preview = &profitPreview{ID: rand.Text(), ExpiresAt: time.Now().Add(2 * time.Minute), Candidate: a, ClosingFee: fee, FeeAllowance: p.config.Fee, MinimumProfit: a.EntryCredit - a.CloseLimit*100 - 2*p.config.Fee}
	return p.preview, nil
}

func (p *profitControl) execute(ctx context.Context, id string) (err error) {
	preview := p.preview
	p.preview = nil // A click consumes its preview even on failure; never replay.
	if preview == nil || preview.ID != id || !time.Now().Before(preview.ExpiresAt) {
		return errors.New("close preview expired or already consumed; request a new preview")
	}
	s := p.read()
	if !s.Halted || s.PendingSelectionMode != "" || s.PendingStrategyPreset != "" || !activationReady(s) {
		return errors.New("pause trading and wait for native readiness before taking profit")
	}
	p.busy.Store(true)
	defer p.busy.Store(false)
	defer func() {
		select {
		case p.wake <- struct{}{}:
		default:
		}
	}()
	p.submitMu.Lock()
	// Selection controls share this mutex. Recheck after acquiring it so a
	// concurrent selection pause cannot be overwritten by close activation.
	latest := p.read()
	if !latest.Halted || latest.PendingSelectionMode != "" || latest.PendingStrategyPreset != "" || !activationReady(latest) {
		p.submitMu.Unlock()
		return errors.New("readiness or selection changed before close authorization")
	}
	err = pauseTrading(p.activation, p.activation.Dir, "prepare one close-only action")
	if err == nil {
		err = p.activation.enablePurpose(id)
	}
	if err == nil {
		err = removeControl(p.activation.Dir, "HALT")
	}
	p.submitMu.Unlock()
	defer func() {
		p.submitMu.Lock()
		err = errors.Join(err, pauseTrading(p.activation, p.activation.Dir, "profit-taking action finished; new entries remain paused"))
		p.submitMu.Unlock()
	}()
	if err != nil {
		return err
	}
	b, err := p.broker(ctx, true)
	if err != nil {
		return err
	}
	b.BeforeSubmit = func() error {
		if err := p.activation.fenceIsolation(); err != nil {
			return err
		}
		if !p.activation.enabled() || p.activation.profitOnly() != id || !time.Now().Before(preview.ExpiresAt) {
			return errors.New("close-only authorization absent or expired")
		}
		if _, err := os.Lstat(filepath.Join(p.activation.Dir, "HALT")); !errors.Is(err, os.ErrNotExist) {
			return errors.New("close paused before submission")
		}
		return nil
	}
	return wheel.ExecuteClose(ctx, p.config, p.state, p.store, b, preview.Candidate, time.Now)
}
