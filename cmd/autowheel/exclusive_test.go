package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	aruntime "autowheel/internal/runtime"
	"autowheel/internal/wheel"
)

func nativeRetirement(t *testing.T) ([]byte, wheel.Config) {
	t.Helper()
	raw, err := os.ReadFile("testdata/retired-tradepulse-native.json")
	if err != nil {
		t.Fatal(err)
	}
	var source struct {
		StateDir string `json:"state_dir"`
	}
	if err = json.Unmarshal(raw, &source); err != nil {
		t.Fatal(err)
	}
	c := wheel.DefaultConfig()
	c.Account, c.TradePulseStateDir = "12345678", source.StateDir
	c.RetiredTradePulse = filepath.Join(c.TradePulseStateDir, "supervisor", "cross-host-handoff-sealed.json")
	return raw, c
}

// The source is actual output from the installed Rust binary, captured in a
// scratch state directory without credentials or broker access. Rehashing here
// compacts the entire original object; it does not use the Go verifier's parser.
func resignRetirement(t *testing.T, raw []byte) []byte {
	t.Helper()
	var source struct {
		Hash string `json:"handoff_hash"`
	}
	if err := json.Unmarshal(raw, &source); err != nil {
		t.Fatal(err)
	}
	unsigned := bytes.Replace(raw, []byte(`"`+source.Hash+`"`), []byte(`""`), 1)
	var compact bytes.Buffer
	if err := json.Compact(&compact, unsigned); err != nil {
		t.Fatal(err)
	}
	return bytes.Replace(raw, []byte(`"`+source.Hash+`"`), []byte(`"`+digestBytes(compact.Bytes())+`"`), 1)
}

func exclusiveFixture(t *testing.T) wheel.Config {
	t.Helper()
	raw, c := nativeRetirement(t)
	old := c.TradePulseStateDir
	c.TradePulseStateDir = t.TempDir()
	c.RetiredTradePulse = filepath.Join(c.TradePulseStateDir, "supervisor", "cross-host-handoff-sealed.json")
	if err := os.Mkdir(filepath.Dir(c.RetiredTradePulse), 0700); err != nil {
		t.Fatal(err)
	}
	raw = bytes.ReplaceAll(raw, []byte(old), []byte(c.TradePulseStateDir))
	if err := os.WriteFile(c.RetiredTradePulse, resignRetirement(t, raw), 0600); err != nil {
		t.Fatal(err)
	}
	writePeerConfig(t, c.TradePulseStateDir, c.Account)
	if err := os.WriteFile(filepath.Join(c.TradePulseStateDir, "daemon.lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open("testdata/retired-tradepulse-native.sqlite3.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	db, err := io.ReadAll(io.LimitReader(gz, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.TradePulseStateDir, "pilot.sqlite3"), db, 0600); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNativeTradePulseRetirementSeal(t *testing.T) {
	raw, c := nativeRetirement(t)
	size, hash, err := verifyNativeSeal(raw, c)
	if err != nil || size != 122880 || hash != "3ce7dc32a56c2e8179489d4ed36f182a62ed531483c7ca3fdfff2ac2385ae255" {
		t.Fatal("actual Rust retirement seal rejected", size, hash, err)
	}
	for name, change := range map[string]func([]byte) []byte{
		"open_lifecycle": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"lifecycles": []`), []byte(`"lifecycles": [{"symbol":"MU","expected_quantity_micros":1000000,"flat":false}]`), 1)
		},
		"pending_order": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"order_intents": []`), []byte(`"order_intents": [{"terminal":false}]`), 1)
		},
		"ambiguous_order": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"ambiguous_symbols": []`), []byte(`"ambiguous_symbols": ["MU"]`), 1)
		},
		"missing_intents": func(b []byte) []byte { return bytes.Replace(b, []byte(`"order_intents": [],`), nil, 1) },
		"wrong_account": func(b []byte) []byte {
			return bytes.Replace(b, []byte(digestBytes([]byte(c.Account))), []byte(strings.Repeat("a", 64)), 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := verifyNativeSeal(resignRetirement(t, change(raw)), c); err == nil {
				t.Fatal("valid checksum bypassed unsafe or missing retirement facts")
			}
		})
	}
	if _, _, err := verifyNativeSeal(bytes.Replace(raw, []byte(`"event_count": 0`), []byte(`"event_count": 1`), 1), c); err == nil {
		t.Fatal("changed native seal passed integrity check")
	}
}

func TestRetiredPeerLifetimeLockAndDrift(t *testing.T) {
	for _, drift := range []string{"none", "config", "seal", "database", "wal", "lock", "service"} {
		t.Run(drift, func(t *testing.T) {
			c := exclusiveFixture(t)
			active := false
			inspect := func() error {
				if active {
					return errors.New("peer became active")
				}
				return nil
			}
			g, err := openRetiredPeer(c, inspect)
			if err != nil {
				t.Fatal(err)
			}
			defer g.close()
			if tradePulseIsolation(c) == nil {
				t.Fatal("retirement without a live lease allowed")
			}
			if err := tradePulseIsolation(c, g); err != nil {
				t.Fatal(err)
			}
			other, err := aruntime.PrivateRead(filepath.Join(c.TradePulseStateDir, "daemon.lock"))
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			if err := syscall.Flock(int(other.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
				t.Fatal("competing writer acquired peer lock")
			}
			switch drift {
			case "config":
				writePeerConfig(t, c.TradePulseStateDir, "87654321")
			case "seal":
				if err := os.Remove(c.RetiredTradePulse); err != nil {
					t.Fatal(err)
				}
			case "database":
				if err := os.WriteFile(filepath.Join(c.TradePulseStateDir, "pilot.sqlite3"), []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			case "wal":
				if err := os.WriteFile(filepath.Join(c.TradePulseStateDir, "pilot.sqlite3-wal"), []byte("unsealed"), 0600); err != nil {
					t.Fatal(err)
				}
			case "lock":
				p := filepath.Join(c.TradePulseStateDir, "daemon.lock")
				if err := os.Rename(p, p+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "service":
				active = true
			}
			if err := g.check(c); (err == nil) != (drift == "none") {
				t.Fatal("retirement drift result", drift, err)
			}
		})
	}
}

func TestRetiredPeerRejectsBusyLockAndDatabaseMismatch(t *testing.T) {
	for _, condition := range []string{"busy", "hash", "wal", "mixed_mode"} {
		t.Run(condition, func(t *testing.T) {
			c := exclusiveFixture(t)
			if condition == "busy" {
				f, err := aruntime.PrivateRead(filepath.Join(c.TradePulseStateDir, "daemon.lock"))
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
					t.Fatal(err)
				}
			}
			if condition == "hash" {
				p := filepath.Join(c.TradePulseStateDir, "pilot.sqlite3")
				raw, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				raw[len(raw)-1] ^= 1
				if err := os.WriteFile(p, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if condition == "mixed_mode" {
				c.CoordinatorSocket = "/tmp/coordinator.sock"
				c.CoordinatorPolicy = strings.Repeat("a", 64)
			}
			if condition == "wal" {
				if err := os.WriteFile(filepath.Join(c.TradePulseStateDir, "pilot.sqlite3-wal"), []byte("unsealed"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if g, err := openRetiredPeer(c, func() error { return nil }); err == nil {
				g.close()
				t.Fatal("unsafe handover admitted", condition)
			}
		})
	}
}

func TestRetirementLossDurablyRevokesActivation(t *testing.T) {
	c := exclusiveFixture(t)
	active := false
	g, err := openRetiredPeer(c, func() error {
		if active {
			return errors.New("service no longer retired")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer g.close()
	a := &activationControl{Capable: true, Config: c, Dir: t.TempDir(), RetiredPeer: g}
	if err := a.enable(); err != nil || !a.enabled() {
		t.Fatal("qualified test activation failed", err)
	}
	active = true
	if a.enabled() || a.fenceIsolation() == nil {
		t.Fatal("lost retirement did not fence activation")
	}
	if _, err := os.Stat(filepath.Join(a.Dir, "HALT")); err != nil {
		t.Fatal("pause not durable", err)
	}
	if _, err := os.Stat(filepath.Join(a.Dir, "LIVE.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("activation not revoked", err)
	}
	active = false
	if a.enabled() {
		t.Fatal("restored retirement restarted trading automatically")
	}
}

func TestRetiredTradePulseServiceRequiresPersistentMask(t *testing.T) {
	good := "LoadState=masked\nActiveState=inactive\nMainPID=0\nUnitFileState=masked\n"
	if err := verifyRetiredService(good); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{strings.Replace(good, "ActiveState=inactive", "ActiveState=active", 1), strings.Replace(good, "UnitFileState=masked", "UnitFileState=masked-runtime", 1), strings.Replace(good, "MainPID=0", "MainPID=123", 1), strings.Replace(good, "LoadState=masked\n", "", 1), good + "MainPID=0\n"} {
		if verifyRetiredService(raw) == nil {
			t.Fatal("unsafe/incomplete service state admitted", raw)
		}
	}
}
