package main

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"autowheel/internal/wheel"
)

func TestInitConservativeAndPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	previous := os.Args
	os.Args = []string{"autowheel", "init", "--config", path}
	t.Cleanup(func() { os.Args = previous })
	if err := run(); err != nil {
		t.Fatal(err)
	}
	c, err := loadConfig(path)
	if err != nil || c.SelectionMode != wheel.SelectionConservative || c.StrategyPreset != "conservative-roi-v1" || c.PutMinDelta != .05 || c.PutMaxDelta != .15 || c.SymbolCollateralLimit() != 2_666_666 || c.Account != "" {
		t.Fatalf("unsafe initial configuration: %+v, %v", c, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("initial configuration must be private")
	}
	if err := run(); err == nil {
		t.Fatal("init replaced an existing configuration")
	}
}

type scanBroker struct {
	active  atomic.Int32
	maximum atomic.Int32
	calls   atomic.Int32
}

func (b *scanBroker) Snapshot(context.Context) (wheel.Snapshot, error) { return wheel.Snapshot{}, nil }
func (b *scanBroker) Review(context.Context, wheel.Candidate) error    { panic("scan submitted review") }
func (b *scanBroker) Submit(context.Context, wheel.Candidate, string) (string, error) {
	panic("scan submitted order")
}
func (b *scanBroker) Quotes(ctx context.Context, _, _ string) ([]wheel.Quote, error) {
	n := b.active.Add(1)
	defer b.active.Add(-1)
	b.calls.Add(1)
	for {
		old := b.maximum.Load()
		if old >= n || b.maximum.CompareAndSwap(old, n) {
			break
		}
	}
	timer := time.NewTimer(10 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, nil
	}
}

func TestThreeScanWorkers(t *testing.T) {
	c := wheel.DefaultConfig()
	b := &scanBroker{}
	_, failures := scan(t.Context(), c, wheel.NewState(), b)
	if len(failures) != 0 || b.calls.Load() != int32(len(c.Symbols)) || b.maximum.Load() != 3 || b.active.Load() != 0 {
		t.Fatalf("calls=%d maximum=%d active=%d failures=%d", b.calls.Load(), b.maximum.Load(), b.active.Load(), len(failures))
	}
}
