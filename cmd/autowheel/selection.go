package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	aruntime "autowheel/internal/runtime"
	"autowheel/internal/sharedclient"
	"autowheel/internal/wheel"
)

type selectionOverride struct {
	Mode wheel.SelectionMode `json:"mode"`
}

func pauseForSelection(a *activationControl, dir string) error {
	err := persistPause(dir, "selection mode change")
	if err != nil || a.Config.CoordinatorSocket == "" {
		return err
	}
	_, err = sharedPermission(a.Config, false)
	if err == nil {
		return nil
	}
	// The observer has no registration to revoke before the initial ownership
	// import. Accept only a fresh, bound confirmation of that exact empty state,
	// after independently confirming the durable local fences.
	observed, statusErr := sharedStatus(a.Config)
	if statusErr != nil || !unregisteredObserver(observed, time.Now()) {
		return errors.Join(err, statusErr)
	}
	halt, haltErr := aruntime.PrivateRead(filepath.Join(dir, "HALT"))
	if haltErr != nil {
		return errors.Join(err, haltErr)
	}
	if closeErr := halt.Close(); closeErr != nil {
		return errors.Join(err, closeErr)
	}
	if _, liveErr := os.Lstat(filepath.Join(dir, "LIVE.json")); !errors.Is(liveErr, os.ErrNotExist) {
		return err
	}
	return nil
}

func unregisteredObserver(s sharedclient.Status, now time.Time) bool {
	age := now.Sub(s.At)
	return !s.Ready && !s.Reconciled && !s.SingleWriter && !s.NewRisk && !s.Management &&
		s.Generation == 0 && s.Epoch == 0 && !s.At.IsZero() && age >= -2*time.Second && age <= 90*time.Second
}

// Operator policy lives in the private writable state directory. Changing it
// pauses the old process; only a restart loads the new immutable configuration.
func loadSelectionOverride(c wheel.Config, dir string) (wheel.Config, error) {
	f, err := aruntime.PrivateRead(filepath.Join(dir, "selection.json"))
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, fmt.Errorf("selection policy unreadable: %w", err)
	}
	defer f.Close()
	var override selectionOverride
	d := json.NewDecoder(io.LimitReader(f, 4096))
	d.DisallowUnknownFields()
	if err = d.Decode(&override); err != nil {
		return c, fmt.Errorf("selection policy invalid: %w", err)
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return c, errors.New("selection policy has trailing content")
	}
	c, err = c.WithSelectionMode(override.Mode)
	if err != nil {
		return c, err
	}
	return c, c.Validate()
}
