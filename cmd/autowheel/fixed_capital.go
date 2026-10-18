package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	aruntime "autowheel/internal/runtime"
	"autowheel/internal/wheel"
)

func rebindCapital(config wheel.Config, dir string, out io.Writer) error {
	return transitionFixedPolicy(config, dir, out, false)
}

func enableTP75(config wheel.Config, dir string, out io.Writer) error {
	return transitionFixedPolicy(config, dir, out, true)
}

func transitionFixedPolicy(config wheel.Config, dir string, out io.Writer, tp75 bool) error {
	halt, err := aruntime.PrivateRead(filepath.Join(dir, "HALT"))
	if err != nil {
		return errors.New("fixed policy transition requires an explicit durable pause")
	}
	if err = halt.Close(); err != nil {
		return err
	}
	if _, err = os.Lstat(filepath.Join(dir, "LIVE.json")); !errors.Is(err, os.ErrNotExist) {
		return errors.New("fixed policy transition requires live authorization revoked")
	}
	j, state, err := aruntime.OpenJournal(dir)
	if err != nil {
		return err
	}
	defer j.Close()
	transition := wheel.RebindFixedCapital
	if tp75 {
		transition = wheel.EnableFixedTP75
	}
	next, err := transition(state, config, time.Now())
	if err != nil {
		return err
	}
	archive := j.ArchiveFixedCapitalState
	if tp75 {
		archive = j.ArchiveFixedPolicyState
	}
	pin, err := archive(state)
	if err != nil {
		return err
	}
	if err = j.Save(next); err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(map[string]any{
		"previous_policy": state.Fixed.Spec.ID, "policy": next.Fixed.Spec.ID,
		"previous_journal_archive_sha256": pin, "previous_capital_limits": state.Fixed.Spec.Limits,
		"capital_limits": next.Fixed.Spec.Limits, "owned_symbols_preserved": len(state.Positions),
		"completed_receipts_preserved": len(state.CompletedOrders), "order_submissions": 0,
		"trading_authorized": false, "fresh_native_readiness_required": true,
	})
}
