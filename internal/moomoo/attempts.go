package moomoo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	aruntime "autowheel/internal/runtime"
	"autowheel/internal/wheel"
)

const maxAttemptRecord = 512 << 10
const maxAttemptRecords = 1024

// This fence is not reachable from the observation daemon. It supplies only
// dispatch durability/replay protection for the future qualified writer. Its
// guard must independently check broker readiness and native activation. Both
// callbacks must be synchronous and must not reenter State methods.
type placementDispatch func(context.Context, PlacementRequest) (int, []byte, error)

type preparedPlacement struct {
	owner *State
	ref   string
	hash  string
	once  *atomic.Bool
}

type recoveredPlacement struct {
	At         time.Time `json:"at"`
	VerifiedAt time.Time `json:"verified_at"`
	OrderID    string    `json:"order_id"`
	Raw        []byte    `json:"order_base64"`
	SHA256     string    `json:"sha256"`
}

type placementAttempt struct {
	Version     int                        `json:"version"`
	Binding     stateBinding               `json:"binding"`
	Terms       OrderIntentTerms           `json:"terms"`
	RequestHash string                     `json:"request_sha256"`
	Phase       string                     `json:"phase"`
	PreparedAt  time.Time                  `json:"prepared_at"`
	AttemptedAt time.Time                  `json:"attempted_at,omitzero"`
	Response    *PlacementResponseEvidence `json:"response,omitzero"`
	Recovery    *recoveredPlacement        `json:"recovery,omitzero"`
}

func placementHash(request PlacementRequest) string {
	h := sha256.New()
	h.Write([]byte(request.Path))
	h.Write([]byte{0})
	h.Write(request.Body)
	return hex.EncodeToString(h.Sum(nil))
}

func (s *State) attemptDirectory() (string, error) {
	if s.closed || s.poisoned || s.journal == nil || !validAccountID(s.binding.AccountID) {
		return "", errors.New("Moomoo attempt fence requires the active bound state lock")
	}
	dir := filepath.Join(filepath.Dir(s.path), "moomoo-order-attempts")
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("Moomoo attempts directory is not private or is a symlink")
	}
	return dir, nil
}

func attemptBytes(record placementAttempt) ([]byte, error) {
	raw, err := json.MarshalIndent(record, "", "  ")
	if err != nil || len(raw)+1 > maxAttemptRecord {
		return nil, errors.New("Moomoo attempt record exceeds its restart limit")
	}
	return append(raw, '\n'), nil
}

func syncAttemptDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func (s *State) readAttempt(dir, ref string) (placementAttempt, error) {
	var record placementAttempt
	if !validSubmissionRef(ref) {
		return record, errors.New("invalid Moomoo attempt reference")
	}
	f, err := aruntime.PrivateRead(filepath.Join(dir, ref+".json"))
	if err != nil {
		return record, err
	}
	defer f.Close()
	raw, err := readBounded(f, maxAttemptRecord)
	if err != nil || privateJSON(bytes.NewReader(raw), maxAttemptRecord, &record) != nil {
		return record, errors.New("invalid private Moomoo attempt record")
	}
	canonical, err := attemptBytes(record)
	request, requestErr := PreparePlacement(record.Terms, s.now())
	if err != nil || requestErr != nil || !bytes.Equal(raw, canonical) || record.Version != 1 || record.Binding != s.binding ||
		record.Terms.AccountID != s.binding.AccountID || record.Terms.Ref != ref || record.RequestHash != placementHash(request) || record.PreparedAt.IsZero() {
		return record, errors.New("Moomoo attempt binding, terms or canonical record differs")
	}
	if record.Response != nil {
		response := record.Response
		digest := sha256.Sum256(response.Body)
		if len(response.Body) > maxPlacementResponse || response.BodySHA256 != hex.EncodeToString(digest[:]) {
			return record, errors.New("Moomoo private placement response integrity differs")
		}
	}
	switch record.Phase {
	case "prepared":
		if !record.AttemptedAt.IsZero() || record.Response != nil || record.Recovery != nil {
			return record, errors.New("inconsistent prepared Moomoo attempt")
		}
	case "rejected":
		if record.AttemptedAt.IsZero() || record.Recovery != nil || !providerRejection(record.Response) {
			return record, errors.New("Moomoo rejected attempt lacks its provider error evidence")
		}
	case "attempt_started", "unknown", "not_submitted", "acknowledged":
		if record.AttemptedAt.IsZero() {
			return record, errors.New("Moomoo attempt lacks its durable dispatch marker")
		}
		if (record.Phase == "attempt_started" || record.Phase == "not_submitted") && record.Response != nil || record.Phase == "unknown" && record.Response == nil {
			return record, errors.New("Moomoo attempt phase contradicts its response evidence")
		}
		if record.Phase == "acknowledged" {
			if record.Recovery != nil {
				proof := record.Recovery
				digest := sha256.Sum256(proof.Raw)
				var row receiptRow
				if proof.At.IsZero() || proof.VerifiedAt.Before(proof.At) || proof.VerifiedAt.Sub(proof.At) > 30*time.Second ||
					record.Response != nil && record.Response.OrderID != "" || len(proof.Raw) > maxPlacementResponse || !validPlacementID(proof.OrderID) ||
					proof.SHA256 != hex.EncodeToString(digest[:]) || exactJSON(proof.Raw, &row) != nil || row.ID != proof.OrderID || row.Remark == nil || *row.Remark != ref {
					return record, errors.New("invalid recovered Moomoo acknowledgement")
				}
				if _, err := row.match(record.Terms, proof.At, proof.VerifiedAt); err != nil {
					return record, errors.New("recovered Moomoo order differs from durable terms")
				}
			} else {
				if record.Response == nil || record.Response.BodyTruncated {
					return record, errors.New("Moomoo acknowledgement evidence is missing")
				}
				decoded, err := DecodePlacementResponse(record.Response.HTTPStatus, record.Response.Body)
				if err != nil || decoded.OrderID != record.Response.OrderID {
					return record, errors.New("Moomoo acknowledgement differs from private response")
				}
			}
		} else if record.Recovery != nil || record.Response != nil && record.Response.OrderID != "" {
			return record, errors.New("unresolved Moomoo attempt claims an acknowledgement")
		}
	default:
		return record, errors.New("unknown Moomoo attempt phase")
	}
	return record, nil
}

// preparePlacement returns an in-memory handle only for a newly durable UUID.
// Restarted/pre-existing prepared records remain unresolved; they cannot be
// adopted for dispatch based on order-history absence or a missing marker.
func (s *State) preparePlacement(terms OrderIntentTerms) (*preparedPlacement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, err := s.attemptDirectory()
	if err != nil {
		return nil, err
	}
	terms.CreatedAt = terms.CreatedAt.UTC()
	request, err := PreparePlacement(terms, s.now())
	if err != nil || terms.AccountID != s.binding.AccountID {
		return nil, errors.New("Moomoo prepared intent does not match the sealed account or valid terms")
	}
	folder, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	entries, readErr := folder.ReadDir(maxAttemptRecords + 1)
	folder.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) || len(entries) >= maxAttemptRecords {
		return nil, errors.New("Moomoo attempt history unavailable or at its retention limit")
	}
	for _, entry := range entries {
		if atomicTemp(entry) {
			continue
		}
		ref := strings.TrimSuffix(entry.Name(), ".json")
		if entry.Name() != ref+".json" || !validSubmissionRef(ref) || entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			return nil, errors.New("unexpected Moomoo attempt file; operator review required")
		}
		record, err := s.readAttempt(dir, ref)
		if err != nil {
			return nil, err
		}
		if ref == terms.Ref || !attemptResolved(record) || s.preparing[ref] {
			return nil, errors.New("Moomoo reference already exists or an earlier attempt is unresolved; never resubmit")
		}
	}
	record := placementAttempt{Version: 1, Binding: s.binding, Terms: terms, RequestHash: placementHash(request), Phase: "prepared", PreparedAt: s.now()}
	raw, err := attemptBytes(record)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, terms.Ref+".json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, errors.New("could not create a fresh Moomoo intent record")
	}
	_, writeErr := f.Write(raw)
	err = errors.Join(writeErr, f.Sync(), f.Close())
	if err != nil || syncAttemptDirectory(dir) != nil || syncAttemptDirectory(filepath.Dir(dir)) != nil {
		// Leave even an incomplete record fenced. No dispatch handle is returned.
		return nil, errors.New("Moomoo intent durability failed; no dispatch permitted")
	}
	if s.preparing == nil {
		s.preparing = map[string]bool{}
	}
	s.preparing[terms.Ref] = true
	return &preparedPlacement{owner: s, ref: terms.Ref, hash: record.RequestHash, once: new(atomic.Bool)}, nil
}

func callPlacement(ctx context.Context, send placementDispatch, request PlacementRequest) (status int, raw []byte, err error) {
	defer func() {
		if recover() != nil {
			status, raw, err = 0, nil, ErrPlacementOutcomeUnknown
		}
	}()
	return send(ctx, request)
}

func checkPlacement(ctx context.Context, guard func(context.Context) error) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("Moomoo preflight failed before dispatch")
		}
	}()
	return guard(ctx)
}

func (s *State) attemptPlacement(ctx context.Context, prepared *preparedPlacement, guard func(context.Context) error, send placementDispatch) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if prepared == nil || prepared.owner != s || prepared.once == nil || guard == nil || send == nil || !prepared.once.CompareAndSwap(false, true) {
		return "", ErrPlacementOutcomeUnknown
	}
	delete(s.preparing, prepared.ref)
	dir, err := s.attemptDirectory()
	if err != nil {
		return "", ErrPlacementOutcomeUnknown
	}
	record, err := s.readAttempt(dir, prepared.ref)
	if err != nil || record.Phase != "prepared" || record.RequestHash != prepared.hash {
		return "", ErrPlacementOutcomeUnknown
	}
	path := filepath.Join(dir, prepared.ref+".json")
	record.Phase, record.AttemptedAt = "attempt_started", s.now()
	if aruntime.AtomicJSON(path, record) != nil {
		return "", ErrPlacementOutcomeUnknown
	}
	guardErr := ctx.Err()
	if guardErr == nil {
		guardErr = checkPlacement(ctx, guard)
	}
	if guardErr != nil || ctx.Err() != nil {
		record.Phase = "not_submitted"
		if aruntime.AtomicJSON(path, record) != nil {
			return "", ErrPlacementOutcomeUnknown
		}
		return "", errors.Join(wheel.ErrNotSubmitted, errors.New("Moomoo preflight denied before dispatch"))
	}
	request, err := PreparePlacement(record.Terms, s.now())
	if err != nil || placementHash(request) != prepared.hash {
		// The transport was never invoked; record that durably.
		record.Phase = "not_submitted"
		if aruntime.AtomicJSON(path, record) != nil {
			return "", ErrPlacementOutcomeUnknown
		}
		return "", errors.Join(wheel.ErrNotSubmitted, errors.New("Moomoo placement could not be rebuilt before sending"))
	}
	status, raw, transportErr := callPlacement(ctx, send, request)
	if errors.Is(transportErr, errTransportNotInvoked) && status == 0 && raw == nil {
		record.Phase = "not_submitted"
		if aruntime.AtomicJSON(path, record) != nil {
			return "", ErrPlacementOutcomeUnknown
		}
		return "", errors.Join(wheel.ErrNotSubmitted, errors.New("Moomoo transport denied before sending"))
	}
	evidence, decodeErr := DecodePlacementResponse(status, raw)
	record.Phase = "unknown"
	if transportErr == nil && decodeErr == nil {
		record.Phase = "acknowledged"
	} else {
		evidence.OrderID = "" // Even ErrNotSubmitted from a dispatcher is unknown.
	}
	record.Response = &evidence
	if _, err := attemptBytes(record); err != nil || aruntime.AtomicJSON(path, record) != nil || record.Phase != "acknowledged" {
		return "", ErrPlacementOutcomeUnknown
	}
	return evidence.OrderID, nil
}

// providerRejection is a complete Moomoo application error envelope, not a
// transport failure, truncated body or server-side (5xx) failure.
func providerRejection(e *PlacementResponseEvidence) bool {
	if e == nil || e.BodyTruncated || e.OrderID != "" || e.HTTPStatus != 200 && (e.HTTPStatus < 400 || e.HTTPStatus > 499) {
		return false
	}
	var envelope map[string]json.RawMessage
	if exactJSON(e.Body, &envelope) != nil || string(envelope["s"]) != `"error"` || envelope["d"] != nil {
		return false
	}
	// -2100 (order confirmation) and -2101 (browser action) ask a human to act;
	// acting could still create the order, so they stay fenced for review.
	for _, key := range []string{"need_order_confirm", "confirm_id", "jump_url"} {
		if envelope[key] != nil {
			return false
		}
	}
	var code json.Number
	if exactJSON(envelope["errcode"], &code) != nil {
		return false
	}
	n, err := code.Int64()
	return err == nil && n != 0 && n != -2100 && n != -2101
}

// rejectPlacement resolves an unknown attempt only when two independent facts
// agree: Moomoo answered with an application error, and a complete account
// audit taken at least a minute later has no order carrying this reference.
// Absence alone never resolves an attempt; an existing order is acknowledged.
func (s *State) rejectPlacement(ref string, audit *OrderAudit, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, err := s.attemptDirectory()
	if err != nil {
		return err
	}
	record, err := s.readAttempt(dir, ref)
	if err != nil || record.Phase != "unknown" || !providerRejection(record.Response) || audit == nil || audit.At.Before(record.AttemptedAt.Add(time.Minute)) {
		return errors.New("Moomoo attempt is not a proven provider rejection")
	}
	if _, err := MatchOrderReceipt(audit, record.Terms, now); !errors.Is(err, errReceiptAbsent) {
		return errors.New("Moomoo rejection audit is incomplete or found an order")
	}
	record.Phase = "rejected"
	if _, err := attemptBytes(record); err != nil || aruntime.AtomicJSON(filepath.Join(dir, ref+".json"), record) != nil {
		return ErrPlacementOutcomeUnknown
	}
	return nil
}

// recoverPlacement records an existing exact broker receipt. It changes only
// transport ambiguity, never ownership, settlement or a permission to replay.
func (s *State) recoverPlacement(ref string, audit *OrderAudit, now time.Time) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, err := s.attemptDirectory()
	if err != nil {
		return "", ErrPlacementOutcomeUnknown
	}
	record, err := s.readAttempt(dir, ref)
	if err != nil || record.AttemptedAt.IsZero() || record.Phase == "not_submitted" {
		return "", ErrPlacementOutcomeUnknown
	}
	if record.Phase == "acknowledged" {
		return "", errors.New("Moomoo attempt already acknowledged; reconcile rather than replay")
	}
	evidence, err := MatchOrderReceipt(audit, record.Terms, now)
	if err != nil || len(evidence.Raw) > maxPlacementResponse || !validPlacementID(evidence.ID) {
		return "", ErrPlacementOutcomeUnknown
	}
	digest := sha256.Sum256(evidence.Raw)
	record.Recovery = &recoveredPlacement{At: audit.At, VerifiedAt: now.UTC(), OrderID: evidence.ID, Raw: bytes.Clone(evidence.Raw), SHA256: hex.EncodeToString(digest[:])}
	record.Phase = "acknowledged"
	if _, err := attemptBytes(record); err != nil || aruntime.AtomicJSON(filepath.Join(dir, ref+".json"), record) != nil {
		return "", ErrPlacementOutcomeUnknown
	}
	return evidence.ID, nil
}
