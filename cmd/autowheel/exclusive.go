package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	aruntime "autowheel/internal/runtime"
	"autowheel/internal/wheel"
)

// The native retirement marker fences TradePulse after AutoWheel exits. The
// lifetime flock also excludes older daemons and closes the service-check race.
// This code only consumes a stopped, sealed peer; it never stops or seals it.
type retiredPeer struct {
	config         wheel.Config
	lock           *os.File
	lockInfo       os.FileInfo
	databaseInfo   os.FileInfo
	sealDigest     string
	peerDigest     string
	inspectService func() error
}

func privateBytes(path string) ([]byte, error) {
	f, err := aruntime.PrivateRead(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil || len(raw) > 1<<20 {
		return nil, errors.New("retirement record exceeds bounds or cannot be read")
	}
	return raw, nil
}

func digestBytes(raw []byte) string { return fmt.Sprintf("%x", sha256.Sum256(raw)) }

func sha256Hex(s string) bool {
	v, err := hex.DecodeString(s)
	return err == nil && len(v) == sha256.Size && hex.EncodeToString(v) == s
}

func sealedDatabaseSidecars(dir string) error {
	for _, suffix := range []string{"-wal", "-journal"} {
		info, err := os.Lstat(filepath.Join(dir, "pilot.sqlite3") + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() || info.Size() != 0 {
			return errors.New("retired database has an unsealed journal or WAL")
		}
	}
	return nil
}

// Native Rust hashes the compact struct in declaration order. Keep raw nested
// values so floating-point history is neither rounded nor reserialized by Go.
func verifyNativeSeal(raw []byte, c wheel.Config) (int64, string, error) {
	fields := []string{"schema_version", "state_dir", "database_path", "database_sha256", "database_bytes", "event_count", "event_anchor_hash", "local_state", "lifecycles", "sealed_at", "handoff_hash"}
	d := json.NewDecoder(bytes.NewReader(raw))
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return 0, "", errors.New("invalid native retirement seal")
	}
	values := make(map[string]json.RawMessage)
	for d.More() {
		tok, err := d.Token()
		key, ok := tok.(string)
		if err != nil || !ok || values[key] != nil {
			return 0, "", errors.New("duplicate or invalid retirement seal field")
		}
		var v json.RawMessage
		if err = d.Decode(&v); err != nil {
			return 0, "", err
		}
		values[key] = v
	}
	if _, err = d.Token(); err != nil {
		return 0, "", err
	}
	var extra any
	if d.Decode(&extra) != io.EOF || len(values) != len(fields) {
		return 0, "", errors.New("incomplete or extended native retirement seal")
	}
	var seal struct {
		Schema       string    `json:"schema_version"`
		StateDir     string    `json:"state_dir"`
		DatabasePath string    `json:"database_path"`
		DatabaseHash string    `json:"database_sha256"`
		DatabaseSize int64     `json:"database_bytes"`
		Anchor       string    `json:"event_anchor_hash"`
		SealedAt     time.Time `json:"sealed_at"`
		Hash         string    `json:"handoff_hash"`
		Local        struct {
			Account    string `json:"expected_account_identity_hash"`
			Lifecycles *[]struct {
				Symbol   string `json:"symbol"`
				Quantity *int64 `json:"expected_quantity_micros"`
				Flat     *bool  `json:"flat"`
			} `json:"lifecycles"`
			Orders *[]struct {
				Terminal *bool `json:"terminal"`
			} `json:"order_intents"`
			Ambiguous *[]string `json:"ambiguous_symbols"`
		} `json:"local_state"`
		Lifecycles *[]struct {
			Symbol   string      `json:"symbol"`
			State    string      `json:"state"`
			Quantity json.Number `json:"current_quantity"`
		} `json:"lifecycles"`
	}
	if err = json.Unmarshal(raw, &seal); err != nil {
		return 0, "", err
	}
	if seal.Schema != "PilotStateHandoffV1" || seal.StateDir != c.TradePulseStateDir || seal.DatabasePath != filepath.Join(c.TradePulseStateDir, "pilot.sqlite3") || seal.DatabaseSize <= 0 || !sha256Hex(seal.DatabaseHash) || !sha256Hex(seal.Anchor) || !sha256Hex(seal.Hash) || seal.SealedAt.IsZero() || seal.SealedAt.After(time.Now().Add(2*time.Second)) || seal.Local.Account != digestBytes([]byte(c.Account)) {
		return 0, "", errors.New("native retirement account/database binding is invalid")
	}
	if c.OrderCoveragePolicy == "exclusive_standard" && !c.ExclusiveOrdersSince.Equal(seal.SealedAt) {
		return 0, "", errors.New("exclusive activity cutoff differs from verified native retirement")
	}
	values["handoff_hash"] = json.RawMessage(`""`)
	var canonical bytes.Buffer
	canonical.WriteByte('{')
	for i, field := range fields {
		if i > 0 {
			canonical.WriteByte(',')
		}
		key, _ := json.Marshal(field)
		canonical.Write(key)
		canonical.WriteByte(':')
		if err = json.Compact(&canonical, values[field]); err != nil {
			return 0, "", err
		}
	}
	canonical.WriteByte('}')
	if digestBytes(canonical.Bytes()) != seal.Hash {
		return 0, "", errors.New("native retirement seal integrity mismatch")
	}
	if seal.Local.Lifecycles == nil || seal.Local.Orders == nil || seal.Local.Ambiguous == nil || seal.Lifecycles == nil || len(*seal.Local.Ambiguous) != 0 {
		return 0, "", errors.New("retirement lifecycle/order evidence is incomplete or ambiguous")
	}
	flat := make(map[string]bool)
	for _, l := range *seal.Local.Lifecycles {
		if l.Symbol == "" || flat[l.Symbol] || l.Flat == nil || !*l.Flat || l.Quantity == nil || *l.Quantity != 0 {
			return 0, "", errors.New("TradePulse lifecycle still requires protection or reconciliation")
		}
		flat[l.Symbol] = true
	}
	if len(flat) != len(*seal.Lifecycles) {
		return 0, "", errors.New("native retirement lifecycle projections disagree")
	}
	for _, l := range *seal.Lifecycles {
		if !flat[l.Symbol] || l.State != "flat" || l.Quantity.String() != "0" && l.Quantity.String() != "0.0" {
			return 0, "", errors.New("native retirement lifecycle is not exactly flat")
		}
		delete(flat, l.Symbol)
	}
	for _, order := range *seal.Local.Orders {
		if order.Terminal == nil || !*order.Terminal {
			return 0, "", errors.New("TradePulse order intent is unresolved")
		}
	}
	return seal.DatabaseSize, seal.DatabaseHash, nil
}

func openRetiredPeer(c wheel.Config, inspectService func() error) (*retiredPeer, error) {
	if err := c.Validate(); err != nil || c.RetiredTradePulse == "" || c.Account == "" || inspectService == nil {
		return nil, errors.New("exclusive retirement configuration is incomplete")
	}
	if err := inspectService(); err != nil {
		return nil, err
	}
	lock, err := aruntime.PrivateRead(filepath.Join(c.TradePulseStateDir, "daemon.lock"))
	if err != nil {
		return nil, err
	}
	g := &retiredPeer{config: c, lock: lock, inspectService: inspectService}
	ok := false
	defer func() {
		if !ok {
			lock.Close()
		}
	}()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errors.New("TradePulse operational lock is still held")
	}
	g.lockInfo, err = lock.Stat()
	if err != nil {
		return nil, err
	}
	raw, err := privateBytes(c.RetiredTradePulse)
	if err != nil {
		return nil, err
	}
	size, hash, err := verifyNativeSeal(raw, c)
	if err != nil {
		return nil, err
	}
	g.sealDigest = digestBytes(raw)
	peer, err := privateBytes(filepath.Join(c.TradePulseStateDir, "config.json"))
	if err != nil {
		return nil, err
	}
	var peerConfig struct {
		Schema  string `json:"schema_version"`
		Account string `json:"expected_account_identity_hash"`
	}
	if json.Unmarshal(peer, &peerConfig) != nil || peerConfig.Schema != "PilotConfigV1" || peerConfig.Account != digestBytes([]byte(c.Account)) {
		return nil, errors.New("retired TradePulse account config differs from handover")
	}
	g.peerDigest = digestBytes(peer)
	if err = sealedDatabaseSidecars(c.TradePulseStateDir); err != nil {
		return nil, err
	}
	db, err := aruntime.PrivateRead(filepath.Join(c.TradePulseStateDir, "pilot.sqlite3"))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	before, err := db.Stat()
	if err != nil || before.Size() != size {
		return nil, errors.New("retired database size mismatch")
	}
	h := sha256.New()
	if _, err = io.Copy(h, db); err != nil {
		return nil, err
	}
	g.databaseInfo, err = db.Stat()
	if err != nil || hex.EncodeToString(h.Sum(nil)) != hash || before.Size() != g.databaseInfo.Size() || before.ModTime() != g.databaseInfo.ModTime() {
		return nil, errors.New("retired database hash mismatch or concurrent change")
	}
	if err = g.check(c); err != nil {
		return nil, err
	}
	ok = true
	return g, nil
}

func (g *retiredPeer) check(c wheel.Config) error {
	if g == nil || g.lock == nil || configDigest(g.config) != configDigest(c) {
		return errors.New("exclusive retirement lease/configuration is absent or changed")
	}
	if _, err := g.lock.Stat(); err != nil {
		return errors.New("exclusive retirement lock was released")
	}
	info, err := os.Lstat(filepath.Join(c.TradePulseStateDir, "daemon.lock"))
	if err != nil || !os.SameFile(info, g.lockInfo) {
		return errors.New("TradePulse operational lock identity changed")
	}
	raw, err := privateBytes(c.RetiredTradePulse)
	if err != nil || digestBytes(raw) != g.sealDigest {
		return errors.New("TradePulse retirement seal changed or disappeared")
	}
	peer, err := privateBytes(filepath.Join(c.TradePulseStateDir, "config.json"))
	if err != nil || digestBytes(peer) != g.peerDigest {
		return errors.New("retired TradePulse configuration changed")
	}
	db, err := os.Lstat(filepath.Join(c.TradePulseStateDir, "pilot.sqlite3"))
	if err != nil || !os.SameFile(db, g.databaseInfo) || db.Size() != g.databaseInfo.Size() || db.ModTime() != g.databaseInfo.ModTime() {
		return errors.New("sealed TradePulse database changed")
	}
	if err = sealedDatabaseSidecars(c.TradePulseStateDir); err != nil {
		return err
	}
	return g.inspectService()
}

func (g *retiredPeer) close() error { return g.lock.Close() }

func retiredTradePulseService() error {
	if runtime.GOOS != "linux" {
		return errors.New("exclusive TradePulse retirement requires Linux systemd")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "/usr/bin/systemctl", "show", "tradepulse.service", "-p", "LoadState", "-p", "ActiveState", "-p", "MainPID", "-p", "UnitFileState").Output()
	if err != nil {
		return errors.New("TradePulse service retirement cannot be inspected")
	}
	return verifyRetiredService(string(raw))
}

func verifyRetiredService(raw string) error {
	want := map[string]string{"LoadState": "masked", "ActiveState": "inactive", "MainPID": "0", "UnitFileState": "masked"}
	for line := range strings.SplitSeq(strings.TrimSpace(raw), "\n") {
		key, value, ok := strings.Cut(line, "=")
		expected, present := want[key]
		if !ok || !present || expected != value {
			return errors.New("TradePulse service must be inactive and persistently masked")
		}
		delete(want, key)
	}
	if len(want) != 0 {
		return errors.New("TradePulse service retirement evidence is incomplete")
	}
	return nil
}
