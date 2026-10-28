package moomoo

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"autowheel/internal/wheel"
)

func readAttemptEntries(dir string) ([]string, error) {
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, ErrPlacementOutcomeUnknown
	}
	f, err := os.Open(dir)
	if err != nil {
		return nil, ErrPlacementOutcomeUnknown
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.IsDir() {
		return nil, ErrPlacementOutcomeUnknown
	}
	entries, err := f.ReadDir(maxAttemptRecords + 1)
	if err != nil && !errors.Is(err, io.EOF) || len(entries) > maxAttemptRecords {
		return nil, ErrPlacementOutcomeUnknown
	}
	var refs []string
	for _, entry := range entries {
		if atomicTemp(entry) {
			continue
		}
		ref := strings.TrimSuffix(entry.Name(), ".json")
		if entry.Name() != ref+".json" || !validSubmissionRef(ref) || entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			return nil, ErrPlacementOutcomeUnknown
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// errKnownUnsent marks an intent whose durable record proves no dispatch: the
// attempt marker is synced before any send, so "prepared" never reached the
// transport, and "not_submitted" was denied before it.
var errKnownUnsent = errors.New("Moomoo intent was durably recorded as never dispatched")

func (s *State) intentAttempt(symbol string, intent wheel.Intent) (placementAttempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, err := s.attemptDirectory()
	if err != nil {
		return placementAttempt{}, err
	}
	record, err := s.readAttempt(dir, intent.Ref)
	if errors.Is(err, os.ErrNotExist) && intent.OrderID == "" && !s.preparing[intent.Ref] {
		// No send happens without a synced prepared record, so a missing one
		// proves preparation never completed and nothing was dispatched.
		return placementAttempt{}, errKnownUnsent
	}
	if err != nil {
		return placementAttempt{}, ErrPlacementOutcomeUnknown
	}
	limit, tif := candidatePlacement(intent.Candidate)
	if intent.Candidate.Symbol != symbol || record.Terms.Code != intent.Candidate.ID || record.Terms.LimitCents != limit || record.Terms.timeInForce() != tif || !record.Terms.CreatedAt.Equal(intent.At) {
		return placementAttempt{}, ErrPlacementOutcomeUnknown
	}
	if intent.OrderID == "" && (record.Phase == "prepared" || record.Phase == "not_submitted" || record.Phase == "rejected") {
		return record, errKnownUnsent
	}
	if record.AttemptedAt.IsZero() || record.Phase == "not_submitted" || record.Phase == "prepared" || record.Phase == "rejected" {
		return placementAttempt{}, ErrPlacementOutcomeUnknown
	}
	if intent.OrderID != "" {
		id := ""
		if record.Response != nil {
			id = record.Response.OrderID
		}
		if record.Recovery != nil {
			id = record.Recovery.OrderID
		}
		if record.Phase != "acknowledged" || id != intent.OrderID {
			return placementAttempt{}, ErrPlacementOutcomeUnknown
		}
	}
	return record, nil
}

// recoverIntents resolves interrupted submissions. An intent whose record
// proves it was never dispatched is dropped, as the engine does when a send is
// denied; one that may have been sent is resolved only by its exact receipt.
func (b *Broker) recoverIntents(ctx context.Context, owned wheel.State) error {
	var unsent []string
	for symbol, intent := range owned.Intents {
		record, err := b.Store.intentAttempt(symbol, intent)
		if errors.Is(err, errKnownUnsent) {
			unsent = append(unsent, symbol)
			continue
		}
		if err != nil {
			return err
		}
		if record.Phase == "acknowledged" {
			continue
		}
		if b.Reader == nil {
			return ErrPlacementOutcomeUnknown
		}
		audit, err := b.Reader.AuditOrders(ctx, b.Config.AccountID, record.Terms.CreatedAt.Add(-time.Minute))
		if err != nil {
			return ErrPlacementOutcomeUnknown
		}
		if _, err := b.Store.recoverPlacement(intent.Ref, audit, b.now()); err != nil {
			// rejectPlacement independently requires the provider's error and the
			// audit's proven absence of this reference; otherwise it stays unknown.
			if b.Store.rejectPlacement(intent.Ref, audit, b.now()) == nil {
				unsent = append(unsent, symbol)
				continue
			}
			return ErrPlacementOutcomeUnknown
		}
	}
	if len(unsent) == 0 {
		return nil
	}
	for _, symbol := range unsent {
		delete(owned.Intents, symbol)
	}
	return b.Store.Save(owned)
}

// Orphaned records also fence the engine after a separately restored journal.
func (s *State) CheckAttemptFence() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.poisoned {
		return ErrPlacementOutcomeUnknown
	}
	dir := filepath.Join(filepath.Dir(s.path), "moomoo-order-attempts")
	entries, err := readAttemptEntries(dir)
	if err != nil {
		return err
	}
	if len(entries) >= maxAttemptRecords {
		return errors.New("Moomoo attempt history is at its retention limit; operator archival is required before trading")
	}
	for _, ref := range entries {
		record, err := s.readAttempt(dir, ref)
		if err != nil {
			return ErrPlacementOutcomeUnknown
		}
		if !attemptResolved(record) || s.preparing[ref] {
			return errors.New("unresolved durable Moomoo attempt blocks new trading")
		}
	}
	return nil
}

// atomicTemp is an interrupted AtomicJSON write. Its rename never happened, so
// the named record still holds the previous durable phase.
func atomicTemp(entry os.DirEntry) bool {
	return strings.HasPrefix(entry.Name(), ".autowheel-") && entry.Type().IsRegular()
}

// attemptResolved is true for acknowledged attempts and for records proving no
// dispatch. Started, unknown or unreadable attempts remain unresolved. Callers
// also treat a prepared record with a live handle in this process as unresolved.
func attemptResolved(record placementAttempt) bool {
	return record.Phase == "acknowledged" || record.Phase == "not_submitted" || record.Phase == "rejected" || record.Phase == "prepared" && record.AttemptedAt.IsZero()
}

// attemptRecorded is false only when the attempt file provably does not exist.
func (s *State) attemptRecorded(ref string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, err := s.attemptDirectory()
	if err != nil || !validSubmissionRef(ref) {
		return true
	}
	_, err = os.Lstat(filepath.Join(dir, ref+".json"))
	return !errors.Is(err, os.ErrNotExist)
}
