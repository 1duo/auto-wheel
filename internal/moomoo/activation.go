package moomoo

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	aruntime "autowheel/internal/runtime"
	"autowheel/internal/wheel"
)

type ActivationBinding struct {
	AccountID        string `json:"account_id"`
	PolicyID         string `json:"policy_id"`
	Release          string `json:"release"`
	ConfigSHA256     string `json:"config_sha256"`
	ExecutableSHA256 string `json:"executable_sha256"`
}

type activationRecord struct {
	Version int               `json:"version"`
	Binding ActivationBinding `json:"binding"`
	At      time.Time         `json:"at"`
}

// CheckReady reads a server-owned, fresh readiness result; it must not reenter
// Activation. The mutex serializes operator pause/activation with actual writes.
type Activation struct {
	Dir        string
	Binding    ActivationBinding
	CheckReady func() error
	mu         sync.Mutex // Serializes activation, pause and dispatch.
	poisoned   atomic.Bool
}

func validDigest(value string) bool {
	raw, err := hex.DecodeString(value)
	return err == nil && len(raw) == 32 && hex.EncodeToString(raw) == value
}

func (a *Activation) valid() bool {
	info, err := os.Lstat(a.Dir)
	return !a.poisoned.Load() && err == nil && info.IsDir() && info.Mode().Perm()&0077 == 0 &&
		validAccountID(a.Binding.AccountID) && a.Binding.PolicyID == wheel.FixedConservativeASAPPolicy &&
		a.Binding.Release != "" && len(a.Binding.Release) <= 128 && validDigest(a.Binding.ConfigSHA256) &&
		validDigest(a.Binding.ExecutableSHA256) && a.CheckReady != nil
}

func (a *Activation) check() error {
	if !a.valid() {
		return errors.New("native Moomoo activation binding unavailable")
	}
	if _, err := os.Lstat(filepath.Join(a.Dir, "HALT")); !errors.Is(err, os.ErrNotExist) {
		return errors.New("Moomoo is durably paused")
	}
	f, err := aruntime.PrivateRead(filepath.Join(a.Dir, "LIVE.json"))
	if err != nil {
		return errors.New("native Moomoo activation record unavailable")
	}
	defer f.Close()
	raw, err := readBounded(f, 4096)
	var record activationRecord
	if err != nil || privateJSON(bytes.NewReader(raw), 4096, &record) != nil {
		return errors.New("invalid native Moomoo activation record")
	}
	canonical, err := json.MarshalIndent(record, "", "  ")
	if err != nil || !bytes.Equal(raw, append(canonical, '\n')) || record.Version != 1 || record.Binding != a.Binding || record.At.IsZero() || record.At.After(time.Now().UTC()) {
		return errors.New("native activation release, configuration, executable or account differs")
	}
	return nil
}

func (a *Activation) Enable() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.valid() {
		return errors.New("native activation requires valid bound software and readiness")
	}
	if err := a.CheckReady(); err != nil {
		return err
	}
	if err := aruntime.AtomicJSON(filepath.Join(a.Dir, "LIVE.json"), activationRecord{1, a.Binding, time.Now().UTC()}); err != nil {
		a.poisoned.Store(true)
		return errors.New("activation persistence failed; restart under pause")
	}
	err := os.Remove(filepath.Join(a.Dir, "HALT"))
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err != nil || syncAttemptDirectory(a.Dir) != nil {
		a.poisoned.Store(true)
		_ = aruntime.AtomicJSON(filepath.Join(a.Dir, "HALT"), map[string]string{"reason": "activation durability uncertain"})
		return errors.New("activation durability uncertain; restart under pause")
	}
	return nil
}

func (a *Activation) Pause(reason string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if reason == "" || len(reason) > 256 {
		reason = "operator pause"
	}
	if err := aruntime.AtomicJSON(filepath.Join(a.Dir, "HALT"), map[string]string{"reason": reason}); err != nil {
		a.poisoned.Store(true)
		return errors.New("pause persistence failed; writes fenced in this process")
	}
	err := os.Remove(filepath.Join(a.Dir, "LIVE.json"))
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err != nil || syncAttemptDirectory(a.Dir) != nil {
		a.poisoned.Store(true)
		return errors.New("activation revocation could not be synced; writes remain paused")
	}
	return nil
}

// Poisoned reports uncertain activation durability; only a restart clears it.
func (a *Activation) Poisoned() bool { return a.poisoned.Load() }

// Enabled is a read-only status check. It does not wait for an in-flight
// dispatch; WithLive repeats the check under the dispatch lock.
func (a *Activation) Enabled() bool {
	return a.check() == nil
}

func (a *Activation) WithLive(fn func() error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if fn == nil {
		return errors.New("native dispatch callback is missing")
	}
	if err := a.check(); err != nil {
		return err
	}
	if err := a.CheckReady(); err != nil {
		return err
	}
	return fn()
}
