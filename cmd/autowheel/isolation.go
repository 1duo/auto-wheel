package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	aruntime "autowheel/internal/runtime"
	"autowheel/internal/wheel"
)

// TradePulse reconciles the entire cash account and may continue protecting its
// positions when new entries are paused. A stopped/blocked peer is therefore
// not proof that its account is available to another strategy. A same-account
// handover requires its verified retirement seal and a lifetime operational lock.
func tradePulseIsolation(c wheel.Config, retired ...*retiredPeer) error {
	if c.RetiredTradePulse != "" {
		if c.CoordinatorSocket != "" || len(retired) != 1 || retired[0] == nil {
			return errors.New("exclusive TradePulse retirement has not been verified")
		}
		return retired[0].check(c)
	}
	if c.CoordinatorSocket != "" {
		return sharedAccountReady(c)
	}
	if c.TradePulseStateDir == "" {
		return errors.New("TradePulse account isolation is not configured")
	}
	f, err := aruntime.PrivateRead(filepath.Join(c.TradePulseStateDir, "config.json"))
	if err != nil {
		return errors.New("TradePulse account isolation cannot be verified; live trading blocked")
	}
	defer f.Close()
	// The peer owns its schema. Read only the stable account binding, permitting
	// its other fields to evolve while rejecting incomplete or malformed JSON.
	var peer struct {
		Schema  string `json:"schema_version"`
		Account string `json:"expected_account_identity_hash"`
	}
	d := json.NewDecoder(io.LimitReader(f, 65536))
	if d.Decode(&peer) != nil {
		return errors.New("TradePulse account isolation record is invalid; live trading blocked")
	}
	var extra any
	digest, err := hex.DecodeString(peer.Account)
	if d.Decode(&extra) != io.EOF || peer.Schema != "PilotConfigV1" || err != nil || len(digest) != sha256.Size || peer.Account != hex.EncodeToString(digest) || c.Account == "" {
		return errors.New("TradePulse account isolation binding is invalid; live trading blocked")
	}
	if peer.Account == fmt.Sprintf("%x", sha256.Sum256([]byte(c.Account))) {
		return errors.New("TradePulse manages this Robinhood account; AutoWheel requires a separate account before live activation")
	}
	return nil
}

// Called under the submission fence. A loss of account isolation durably
// revokes AutoWheel's authorization; restoring the peer file cannot restart it.
func (a *activationControl) fenceIsolation() error {
	err := tradePulseIsolation(a.Config, a.RetiredPeer)
	if err == nil {
		return nil
	}
	if _, statErr := os.Lstat(filepath.Join(a.Dir, "LIVE.json")); errors.Is(statErr, os.ErrNotExist) {
		return err
	}
	if saveErr := aruntime.AtomicJSON(filepath.Join(a.Dir, "HALT"), map[string]string{"reason": "account isolation lost"}); saveErr != nil {
		return errors.Join(err, errors.New("account isolation pause could not be persisted"))
	}
	if removeErr := removeControl(a.Dir, "LIVE.json"); removeErr != nil {
		return errors.Join(err, errors.New("account isolation activation revocation could not be persisted"))
	}
	return err
}
