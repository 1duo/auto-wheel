package moomoo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	aruntime "autowheel/internal/runtime"
	"autowheel/internal/wheel"
)

const nextAttemptRef = "738d9553-7520-49b0-b691-5662506e6c66"

func attemptFixture(t *testing.T) (*State, Config, string, OrderIntentTerms) {
	t.Helper()
	terms, _, _, _ := receiptFixture(t)
	c := DefaultConfig()
	c.AccountID, c.Strategy.Account = terms.AccountID, terms.AccountID
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := OpenState(dir, c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, c, dir, terms
}

func mustPrepareAttempt(t *testing.T, s *State, terms OrderIntentTerms) *preparedPlacement {
	t.Helper()
	handle, err := s.preparePlacement(terms)
	if err != nil {
		t.Fatal(err)
	}
	return handle
}

func mustReadAttempt(t *testing.T, s *State, ref string) placementAttempt {
	t.Helper()
	record, err := s.readAttempt(filepath.Join(filepath.Dir(s.path), "moomoo-order-attempts"), ref)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func attemptGuard(context.Context) error { return nil }

func TestAttemptDurableSingleDispatch(t *testing.T) {
	s, c, dir, terms := attemptFixture(t)
	handle := mustPrepareAttempt(t, s, terms)
	copy := *handle // Copies share the once flag; neither may dispatch twice.
	if mustReadAttempt(t, s, terms.Ref).Phase != "prepared" {
		t.Fatal("intent was not durable before returning its handle")
	}
	response := []byte(`{"s":"ok","d":{"order_id":"broker-id"}}`)
	var calls atomic.Int32
	send := func(_ context.Context, request PlacementRequest) (int, []byte, error) {
		calls.Add(1)
		raw, err := os.ReadFile(filepath.Join(dir, "moomoo-order-attempts", terms.Ref+".json"))
		var record placementAttempt
		if err != nil || json.Unmarshal(raw, &record) != nil || record.Phase != "attempt_started" || record.AttemptedAt.IsZero() {
			t.Error("dispatch preceded its durable attempt marker")
		}
		want, _ := PreparePlacement(terms, time.Now())
		if request.Path != want.Path || !bytes.Equal(request.Body, want.Body) {
			t.Error("dispatch changed the account or prepared terms")
		}
		if peer, err := OpenState(dir, c); err == nil {
			peer.Close()
			t.Error("another process could acquire state during dispatch")
		}
		return 200, response, nil
	}
	type result struct {
		id  string
		err error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for _, prepared := range []*preparedPlacement{handle, &copy} {
		wg.Go(func() {
			id, err := s.attemptPlacement(t.Context(), prepared, attemptGuard, send)
			results <- result{id, err}
		})
	}
	wg.Wait()
	close(results)
	acknowledged := 0
	for result := range results {
		if result.err == nil && result.id == "broker-id" {
			acknowledged++
		} else if result.id != "" || !errors.Is(result.err, ErrPlacementOutcomeUnknown) {
			t.Fatalf("duplicate outcome was not fenced: %v", result.err)
		}
	}
	record := mustReadAttempt(t, s, terms.Ref)
	if calls.Load() != 1 || acknowledged != 1 || record.Phase != "acknowledged" || record.Response == nil || !bytes.Equal(record.Response.Body, response) {
		t.Fatal("single dispatch acknowledgement was not privately durable")
	}
	if _, err := s.preparePlacement(terms); err == nil {
		t.Fatal("acknowledged reference was reusable")
	}
	terms.Ref = nextAttemptRef
	mustPrepareAttempt(t, s, terms)
}

func TestAttemptDeniedBeforeDispatch(t *testing.T) {
	for _, kind := range []string{"denied", "cancelled", "guard_panic"} {
		t.Run(kind, func(t *testing.T) {
			s, _, _, terms := attemptFixture(t)
			handle := mustPrepareAttempt(t, s, terms)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			guard := func(context.Context) error {
				if kind == "guard_panic" {
					panic("private guard details must not escape")
				}
				return errors.New("denied")
			}
			if kind == "cancelled" {
				cancel()
			}
			calls := 0
			id, err := s.attemptPlacement(ctx, handle, guard, func(context.Context, PlacementRequest) (int, []byte, error) {
				calls++
				return 200, nil, nil
			})
			if id != "" || !errors.Is(err, wheel.ErrNotSubmitted) || calls != 0 || mustReadAttempt(t, s, terms.Ref).Phase != "not_submitted" {
				t.Fatalf("preflight denial not durable/known unsent: %v", err)
			}
			terms.Ref = nextAttemptRef
			mustPrepareAttempt(t, s, terms)
		})
	}
}

func TestAttemptUnknownNeverReplayed(t *testing.T) {
	for _, kind := range []string{"transport", "misleading_unsent", "challenge", "panic", "success_with_transport_error", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			s, c, dir, terms := attemptFixture(t)
			handle := mustPrepareAttempt(t, s, terms)
			calls := 0
			send := func(context.Context, PlacementRequest) (int, []byte, error) {
				calls++
				switch kind {
				case "misleading_unsent":
					return 0, nil, wheel.ErrNotSubmitted
				case "challenge":
					return 200, []byte(`{"s":"need_order_confirm","confirm_id":"private"}`), nil
				case "panic":
					panic("private dispatch details must not escape")
				case "success_with_transport_error":
					return 200, []byte(`{"s":"ok","d":{"order_id":"broker-id"}}`), errors.New("late transport error")
				case "oversized":
					return 200, bytes.Repeat([]byte("x"), maxPlacementResponse+1), nil
				default:
					return 0, nil, errors.New("timeout")
				}
			}
			for range 2 {
				id, err := s.attemptPlacement(t.Context(), handle, attemptGuard, send)
				if id != "" || !errors.Is(err, ErrPlacementOutcomeUnknown) || errors.Is(err, wheel.ErrNotSubmitted) {
					t.Fatalf("dispatch uncertainty became replay permission: %v", err)
				}
			}
			if calls != 1 || mustReadAttempt(t, s, terms.Ref).Phase != "unknown" {
				t.Fatal("unknown attempt was replayed or not retained")
			}
			s.Close()
			reopened, err := OpenState(dir, c)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if _, err := reopened.attemptPlacement(t.Context(), handle, attemptGuard, send); !errors.Is(err, ErrPlacementOutcomeUnknown) {
				t.Fatal("prior process handle was adopted")
			}
			terms.Ref = nextAttemptRef
			if _, err := reopened.preparePlacement(terms); err == nil || calls != 1 {
				t.Fatal("restart allowed a replacement reference for an unresolved attempt")
			}
		})
	}
}

// The attempt marker is synced before any send, so a prepared record proves no
// dispatch. While its handle is live in this process it still fences; after a
// restart it can never be dispatched, and it no longer blocks new trading.
func TestAttemptOrphanedPreparationIsKnownUnsentAfterRestart(t *testing.T) {
	s, c, dir, terms := attemptFixture(t)
	handle := mustPrepareAttempt(t, s, terms)
	_, now, audit, _ := receiptFixture(t)
	if _, err := s.recoverPlacement(terms.Ref, audit, now); err == nil {
		t.Fatal("a receipt appropriated a never-attempted intent")
	}
	next := terms
	next.Ref = nextAttemptRef
	if _, err := s.preparePlacement(next); err == nil || s.CheckAttemptFence() == nil {
		t.Fatal("a second preparation ran beside a live dispatch handle")
	}
	s.Close()
	reopened, err := OpenState(dir, c)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.attemptPlacement(t.Context(), handle, attemptGuard, nil); err == nil {
		t.Fatal("orphaned handle was adopted after restart")
	}
	if _, err := reopened.recoverPlacement(terms.Ref, audit, now); err == nil {
		t.Fatal("a receipt appropriated an orphaned preparation after restart")
	}
	if err := reopened.CheckAttemptFence(); err != nil {
		t.Fatal("a never-dispatched preparation fenced trading after restart", err)
	}
	if _, err := reopened.preparePlacement(next); err != nil {
		t.Fatal("a never-dispatched preparation blocked a fresh reference", err)
	}
}

func TestAttemptGTCDurationSurvivesRecovery(t *testing.T) {
	s, _, _, terms := attemptFixture(t)
	terms.TimeInForce = "GTC"
	handle := mustPrepareAttempt(t, s, terms)
	_, err := s.attemptPlacement(t.Context(), handle, attemptGuard, func(_ context.Context, request PlacementRequest) (int, []byte, error) {
		var fields map[string]string
		if json.Unmarshal(request.Body, &fields) != nil || fields["time_in_force"] != "GTC" {
			t.Error("durable dispatch lost its explicit duration")
		}
		return 0, nil, errors.New("timeout")
	})
	if !errors.Is(err, ErrPlacementOutcomeUnknown) {
		t.Fatal("lost response cleared the GTC fence")
	}
	_, now, audit, row := receiptFixture(t)
	if _, err := s.recoverPlacement(terms.Ref, audit, now); err == nil {
		t.Fatal("different duration recovered a GTC attempt")
	}
	row["time_in_force"] = "GTC"
	audit.Recent = []OrderEvidence{receiptEvidence(t, row)}
	if id, err := s.recoverPlacement(terms.Ref, audit, now); err != nil || id != "broker-id" {
		t.Fatalf("exact GTC recovery failed: %v", err)
	}
	if record := mustReadAttempt(t, s, terms.Ref); record.Phase != "acknowledged" || record.Terms.TimeInForce != "GTC" {
		t.Fatal("durable recovery lost the requested duration")
	}
}

func TestAttemptRecoveryRequiresExactReceipt(t *testing.T) {
	for _, kind := range []string{"exact", "missing", "foreign", "stale", "conflicting"} {
		t.Run(kind, func(t *testing.T) {
			s, _, _, terms := attemptFixture(t)
			handle := mustPrepareAttempt(t, s, terms)
			s.attemptPlacement(t.Context(), handle, attemptGuard, func(context.Context, PlacementRequest) (int, []byte, error) {
				return 0, nil, errors.New("timeout")
			})
			_, now, audit, row := receiptFixture(t)
			// A response may observe an update after the audit's start clock.
			row["updated_time"] = now.Add(-500 * time.Millisecond).UnixMicro()
			audit.Recent = []OrderEvidence{receiptEvidence(t, row)}
			switch kind {
			case "missing":
				audit.Recent = []OrderEvidence{}
			case "foreign":
				audit.AccountID = "123456"
			case "stale":
				now = now.Add(time.Minute)
			case "conflicting":
				row["price"] = "1.26"
				audit.History = []OrderEvidence{receiptEvidence(t, row)}
			}
			id, err := s.recoverPlacement(terms.Ref, audit, now)
			record := mustReadAttempt(t, s, terms.Ref)
			if kind == "exact" {
				if err != nil || id != "broker-id" || record.Phase != "acknowledged" || record.Recovery == nil || !record.Recovery.VerifiedAt.Equal(now) {
					t.Fatalf("exact recovery not durable: %v", err)
				}
			} else if id != "" || !errors.Is(err, ErrPlacementOutcomeUnknown) || record.Phase != "unknown" {
				t.Fatalf("incomplete/conflicting recovery cleared the fence: %v", err)
			}
		})
	}
}

func TestAttemptPersistenceFailureAfterDispatchStaysFenced(t *testing.T) {
	s, c, dir, terms := attemptFixture(t)
	handle := mustPrepareAttempt(t, s, terms)
	path := filepath.Join(dir, "moomoo-order-attempts", terms.Ref+".json")
	saved := filepath.Join(dir, "saved-attempt.json")
	calls := 0
	id, err := s.attemptPlacement(t.Context(), handle, attemptGuard, func(context.Context, PlacementRequest) (int, []byte, error) {
		calls++
		if err := os.Rename(path, saved); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		return 200, []byte(`{"s":"ok","d":{"order_id":"broker-id"}}`), nil
	})
	if id != "" || !errors.Is(err, ErrPlacementOutcomeUnknown) || errors.Is(err, wheel.ErrNotSubmitted) || calls != 1 {
		t.Fatalf("unpersisted acknowledgement escaped: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(saved, path); err != nil {
		t.Fatal(err)
	}
	s.Close()
	reopened, err := OpenState(dir, c)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if mustReadAttempt(t, reopened, terms.Ref).Phase != "attempt_started" {
		t.Fatal("failed acknowledgement lost the dispatch marker")
	}
	terms.Ref = nextAttemptRef
	if _, err := reopened.preparePlacement(terms); err == nil {
		t.Fatal("failed persistence permitted a replacement attempt")
	}
}

func TestAttemptRejectsAlteredOrNonprivateState(t *testing.T) {
	for _, kind := range []string{"binding", "terms", "alias", "nonprivate", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			s, _, dir, terms := attemptFixture(t)
			handle := mustPrepareAttempt(t, s, terms)
			path := filepath.Join(dir, "moomoo-order-attempts", terms.Ref+".json")
			record := mustReadAttempt(t, s, terms.Ref)
			switch kind {
			case "binding", "terms":
				if kind == "binding" {
					record.Binding.AccountID = "123456"
				} else {
					record.Terms.LimitCents++
				}
				if err := aruntime.AtomicJSON(path, record); err != nil {
					t.Fatal(err)
				}
			case "alias":
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, bytes.Replace(raw, []byte(`"version"`), []byte(`"VERSION"`), 1), 0600); err != nil {
					t.Fatal(err)
				}
			case "nonprivate":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := filepath.Join(dir, "symlink-target.json")
				if err := os.Rename(path, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			if _, err := s.attemptPlacement(t.Context(), handle, attemptGuard, func(context.Context, PlacementRequest) (int, []byte, error) {
				calls++
				return 200, nil, nil
			}); err == nil || calls != 0 {
				t.Fatal("altered/private state reached dispatch")
			}
			terms.Ref = nextAttemptRef
			if _, err := s.preparePlacement(terms); err == nil {
				t.Fatal("altered/private state allowed a replacement intent")
			}
		})
	}
}
