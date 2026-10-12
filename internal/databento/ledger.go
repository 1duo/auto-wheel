package databento

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// MaxLifetimeNano is the cumulative $10 pilot authorization in nanodollars.
// It applies only to legacy capped ledgers (version 1).
const MaxLifetimeNano = int64(10_000_000_000)

// ledgerVersion is the legacy capped ledger schema version.
const ledgerVersion = 1

// uncappedLedgerVersion is the explicit durable no-spending-limit policy version.
const uncappedLedgerVersion = 2

// uncappedLifetimeNano is the lifetime field for the uncapped policy.
// Version 2 ledgers must carry lifetime_nano 0.
const uncappedLifetimeNano = int64(0)

// uncappedAuthorizationMode is the only authorization_mode accepted with
// version 2. It marks user-authorized removal of the cumulative $10 cap.
const uncappedAuthorizationMode = "uncapped"

// maxInt64Headroom is the representable int64 capacity used for uncapped
// Remaining and overflow checks. It is not a budget.
const maxInt64Headroom = int64(1<<63 - 1)

// Keep the durable file within the reader's bound. Each pending entry needs
// at most 128 extra encoded bytes for its eventual hash/positive int64 count.
// Two MiB accommodates the existing acquisition history plus the frozen
// multiyear scopes without deleting reservations or changing authorization.
const maxLedgerFileBytes = 2 << 20

// allowedBillableEndpoints lists endpoints that may consume the ledger.
// Range is the streaming endpoint; batch.submit_job is the bulk endpoint
// accepted only for uncapped (v2) ledgers. Legacy v1 remains capped and
// cannot reserve or reopen batch entries.
const BatchSubmitEndpoint = "/batch.submit_job"

var allowedBillableEndpoints = []string{RangeEndpoint, BatchSubmitEndpoint}

// Reservation is one durable charge against the lifetime authorization.
// There is no release: failed or interrupted operations stay charged.
type Reservation struct {
	OperationID  string    `json:"operation_id"`
	QueryHash    string    `json:"query_hash"`
	Endpoint     string    `json:"endpoint"`
	CostNano     int64     `json:"cost_nano"`
	ReservedNano int64     `json:"reserved_nano"`
	At           time.Time `json:"at"`
	ResponseHash string    `json:"response_hash"`
	Status       string    `json:"status"`
	DataSHA256   string    `json:"data_sha256,omitempty"`
	Bytes        int64     `json:"bytes,omitempty"`
}

type ledgerState struct {
	Version           int           `json:"version"`
	LifetimeNano      int64         `json:"lifetime_nano"`
	AuthorizationMode string        `json:"authorization_mode,omitempty"`
	Reservations      []Reservation `json:"reservations"`
}

// Ledger is a process-exclusive durable record of billable reservations.
type Ledger struct {
	mu       sync.Mutex
	path     string
	lockFile *os.File
	state    ledgerState
	failed   bool
}

// ReserveAmount returns the durable charge for a quoted cost: cost plus a
// 10% margin rounded up. Zero costs reserve nothing. It never uses floats.
func ReserveAmount(costNano int64) (int64, error) {
	if costNano < 0 {
		return 0, errors.New("databento cost must be nonnegative")
	}
	if costNano == 0 {
		return 0, nil
	}
	margin := costNano / 10
	if costNano%10 != 0 {
		margin++
	}
	if costNano > int64(1<<63-1)-margin {
		return 0, errors.New("databento reservation overflow")
	}
	reserved := costNano + margin
	if reserved < costNano {
		return 0, errors.New("databento reservation overflow")
	}
	return reserved, nil
}

// OpenLedger opens or creates the ledger at path and holds an exclusive
// non-blocking flock until Close. Path must be absolute with no symlink for
// the file or its parent; the parent must be private and the file mode 0600.
// A missing file is created with the default capped policy (version 1,
// lifetime $10). A migrated file persists uncapped (version 2) across reopen.
func OpenLedger(path string) (*Ledger, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("databento ledger path must be absolute")
	}
	dir := filepath.Dir(path)
	dinfo, err := os.Lstat(dir)
	if err != nil || !dinfo.IsDir() || dinfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("databento ledger directory unavailable or insecure")
	}
	if dinfo.Mode().Perm()&0077 != 0 {
		return nil, errors.New("databento ledger directory must be private")
	}
	var dirStat unix.Stat_t
	if unix.Lstat(dir, &dirStat) != nil || int(dirStat.Uid) != os.Getuid() {
		return nil, errors.New("databento ledger directory must be owned by current user")
	}
	if finfo, err := os.Lstat(path); err == nil {
		if finfo.Mode()&os.ModeSymlink != 0 || !finfo.Mode().IsRegular() {
			return nil, errors.New("databento ledger must be a regular file, not a symlink")
		}
		if finfo.Mode().Perm() != 0600 {
			return nil, errors.New("databento ledger file must have permissions 0600")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("databento ledger unavailable")
	}
	lockPath := path + ".lock"
	lockFD, err := unix.Open(lockPath, unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, errors.New("databento ledger lock unavailable")
	}
	lock := os.NewFile(uintptr(lockFD), lockPath)
	var lockStat unix.Stat_t
	if unix.Fstat(lockFD, &lockStat) != nil || int(lockStat.Uid) != os.Getuid() || lockStat.Nlink != 1 || lockStat.Mode&unix.S_IFMT != unix.S_IFREG {
		lock.Close()
		return nil, errors.New("databento ledger lock insecure")
	}
	if err := unix.Flock(lockFD, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("another databento process owns this ledger")
	}
	if err := unix.Fchmod(lockFD, 0600); err != nil {
		unix.Flock(lockFD, unix.LOCK_UN)
		lock.Close()
		return nil, errors.New("databento ledger lock unavailable")
	}
	st, err := readLedgerFile(path)
	if err != nil {
		unix.Flock(lockFD, unix.LOCK_UN)
		lock.Close()
		return nil, err
	}
	l := &Ledger{path: path, lockFile: lock, state: st}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		if err := writeLedgerFile(path, st); err != nil {
			l.Close()
			return nil, err
		}
	}
	return l, nil
}

func readLedgerFile(path string) (ledgerState, error) {
	empty := ledgerState{Version: ledgerVersion, LifetimeNano: MaxLifetimeNano}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || err == unix.ENOENT {
			return empty, nil
		}
		return ledgerState{}, errors.New("databento ledger unavailable")
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return ledgerState{}, errors.New("databento ledger must be a regular file with permissions 0600")
	}
	var fileStat unix.Stat_t
	if unix.Fstat(fd, &fileStat) != nil || int(fileStat.Uid) != os.Getuid() || fileStat.Nlink != 1 {
		return ledgerState{}, errors.New("databento ledger ownership invalid")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxLedgerFileBytes+1))
	if err != nil || len(raw) > maxLedgerFileBytes || rejectDuplicateKeys(raw) != nil {
		return ledgerState{}, errors.New("databento ledger is malformed or oversized")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var st ledgerState
	if err := dec.Decode(&st); err != nil {
		return ledgerState{}, errors.New("databento ledger is malformed")
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return ledgerState{}, errors.New("databento ledger has trailing content")
	}
	if err := validateLedgerState(st); err != nil {
		return ledgerState{}, err
	}
	return st, nil
}

func rejectDuplicateKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var walk func() error
	walk = func() error {
		token, err := dec.Token()
		if err != nil {
			return err
		}
		delim, structured := token.(json.Delim)
		if !structured {
			return nil
		}
		seen := make(map[string]bool)
		for dec.More() {
			if delim == '{' {
				key, err := dec.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return errors.New("duplicate JSON field")
				}
				seen[name] = true
			}
			if err := walk(); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func validateLedgerState(st ledgerState) error {
	capped := st.Version == ledgerVersion && st.LifetimeNano == MaxLifetimeNano && st.AuthorizationMode == ""
	uncapped := st.Version == uncappedLedgerVersion && st.LifetimeNano == uncappedLifetimeNano && st.AuthorizationMode == uncappedAuthorizationMode
	if !capped && !uncapped {
		return errors.New("databento ledger authorization unsupported")
	}
	if len(st.Reservations) > 100000 {
		return errors.New("databento ledger too large")
	}
	seen := make(map[string]Reservation, len(st.Reservations))
	var total int64
	for _, r := range st.Reservations {
		if err := validateReservation(r); err != nil {
			return err
		}
		if capped && r.Endpoint == BatchSubmitEndpoint {
			return errors.New("databento batch requires uncapped ledger")
		}
		if r.Endpoint == BatchSubmitEndpoint && r.Status != "reserved" && r.Status != "acquired" {
			return errors.New("databento batch acknowledgement is not complete")
		}
		if prev, dup := seen[r.OperationID]; dup {
			if prev != r {
				return errors.New("databento ledger has conflicting duplicate reservation")
			}
			return errors.New("databento ledger has duplicate reservation")
		}
		seen[r.OperationID] = r
		if r.ReservedNano < 0 {
			return errors.New("databento reservation cost invalid")
		}
		if capped {
			if r.ReservedNano > MaxLifetimeNano {
				return errors.New("databento reservation exceeds authorization")
			}
			if total > MaxLifetimeNano-r.ReservedNano {
				return errors.New("databento ledger total exceeds authorization")
			}
			total += r.ReservedNano
			if total < 0 || total > MaxLifetimeNano {
				return errors.New("databento ledger total exceeds authorization")
			}
		} else {
			if total > maxInt64Headroom-r.ReservedNano {
				return errors.New("databento ledger total overflow")
			}
			total += r.ReservedNano
			if total < 0 {
				return errors.New("databento ledger total overflow")
			}
		}
	}
	return nil
}

func validateReservation(r Reservation) error {
	if len(r.OperationID) != 64 || len(r.QueryHash) != 64 || len(r.ResponseHash) != 64 {
		return errors.New("databento reservation identity malformed")
	}
	for _, s := range []string{r.OperationID, r.QueryHash, r.ResponseHash} {
		if _, err := hex.DecodeString(s); err != nil {
			return errors.New("databento reservation identity malformed")
		}
	}
	if !slices.Contains(allowedBillableEndpoints, r.Endpoint) {
		return errors.New("databento reservation endpoint unsupported")
	}
	if r.CostNano < 0 || r.ReservedNano < 0 {
		return errors.New("databento reservation cost invalid")
	}
	if r.Status != "reserved" && r.Status != "complete" && r.Status != "acquired" {
		return errors.New("databento reservation status invalid")
	}
	if r.Status == "acquired" && r.Endpoint != BatchSubmitEndpoint {
		return errors.New("databento acquired status requires batch")
	}
	if r.Status == "complete" || r.Status == "acquired" {
		if len(r.DataSHA256) != 64 || r.Bytes <= 0 {
			return errors.New("databento completed receipt invalid")
		}
		if _, err := hex.DecodeString(r.DataSHA256); err != nil {
			return errors.New("databento completed receipt invalid")
		}
	} else if r.DataSHA256 != "" || r.Bytes != 0 {
		return errors.New("databento pending receipt invalid")
	}
	expect, err := ReserveAmount(r.CostNano)
	if err != nil || expect != r.ReservedNano {
		return errors.New("databento reservation margin mismatch")
	}
	// Authorization-independent: the legacy $10 individual cap is enforced in
	// validateLedgerState and Reserve for capped ledgers only, so uncapped
	// ledgers may hold larger exact-integer reservations.
	if r.At.IsZero() || r.At.After(time.Now().Add(2*time.Minute)) {
		return errors.New("databento reservation time invalid")
	}
	return nil
}

// Find returns the reservation for operationID, if present.
func (l *Ledger) Find(operationID string) (Reservation, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lockFile == nil {
		return Reservation{}, false
	}
	for _, r := range l.state.Reservations {
		if r.OperationID == operationID {
			return r, true
		}
	}
	return Reservation{}, false
}

// TotalReserved returns the cumulative reserved nanodollars.
func (l *Ledger) TotalReserved() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	var total int64
	for _, r := range l.state.Reservations {
		total += r.ReservedNano
	}
	return total
}

// IsUncapped reports whether the ledger carries the explicit durable
// no-spending-limit policy (version 2, lifetime 0, authorization_mode
// "uncapped"). Use it to distinguish a real $10 budget from uncapped integer
// headroom returned by Remaining.
func (l *Ledger) IsUncapped() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state.Version == uncappedLedgerVersion && l.state.LifetimeNano == uncappedLifetimeNano && l.state.AuthorizationMode == uncappedAuthorizationMode
}

// EnableUncapped performs the user-authorized removal of the cumulative $10
// spending cap. It holds the same ledger lock, rejects a closed or poisoned
// handle, atomically rewrites the same single private ledger file with
// version 2, lifetime_nano 0 and authorization_mode "uncapped", preserves
// every reservation field (including old unknown reserved holds with
// no repricing, reset, or retry), and is idempotent when already uncapped.
// A failed write poisons this handle; reopen and inspect the on-disk evidence.
func (l *Ledger) EnableUncapped() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lockFile == nil || l.failed {
		return errors.New("databento ledger closed or requires recovery")
	}
	if l.state.Version == uncappedLedgerVersion && l.state.LifetimeNano == uncappedLifetimeNano && l.state.AuthorizationMode == uncappedAuthorizationMode {
		return nil
	}
	if l.state.Version != ledgerVersion || l.state.LifetimeNano != MaxLifetimeNano || l.state.AuthorizationMode != "" {
		return errors.New("databento ledger authorization unsupported")
	}
	updated := ledgerState{
		Version:           uncappedLedgerVersion,
		LifetimeNano:      uncappedLifetimeNano,
		AuthorizationMode: uncappedAuthorizationMode,
		Reservations:      slices.Clone(l.state.Reservations),
	}
	if err := validateLedgerState(updated); err != nil {
		return err
	}
	if err := writeLedgerFile(l.path, updated); err != nil {
		l.failed = true
		return err
	}
	l.state = updated
	return nil
}

// Remaining returns the unreserved lifetime authorization for capped ledgers.
// For uncapped ledgers there is no spending limit; it returns the
// representable int64 headroom (MaxInt64 minus total, floored at 0), which is
// integer capacity, not a budget. Use IsUncapped to distinguish the two.
func (l *Ledger) Remaining() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	var total int64
	for _, r := range l.state.Reservations {
		total += r.ReservedNano
	}
	if l.state.Version == uncappedLedgerVersion && l.state.LifetimeNano == uncappedLifetimeNano && l.state.AuthorizationMode == uncappedAuthorizationMode {
		if total < 0 {
			return 0
		}
		return max(maxInt64Headroom-total, 0)
	}
	return max(MaxLifetimeNano-total, 0)
}

// Reserve durably charges operationID before any billable HTTP is sent.
// The same canonical operation never sends twice: an existing operationID is
// rejected, including a previously failed request, and its field values are
// immutable. Unknown attempts keep their full hold forever with status
// "reserved"; there is no release, refund, or inferred completion. A distinct
// cost-qualified client operation may reserve within the remaining holds. The
// client reserves its conservative record-cap basis plus a retained margin;
// request-wide record and price limits bound data usage under that hold.
// Actual billed usage remains unverified. Any field mutation, excessive cost, or
// malformed state is rejected. For capped ledgers the cumulative $10 limit and
// the legacy per-reservation $10 cap apply; for uncapped ledgers the cumulative
// limit is removed while exact integers, margin, MaxInt64 sum/individual
// overflow checks, no retries, durable journal, ownership and 1MiB evidence
// capacity remain. Reserve preserves the stored authorization header instead
// of recreating version 1.
func (l *Ledger) Reserve(operationID, queryHash, endpoint string, costNano int64, at time.Time, responseHash string) (Reservation, error) {
	if len(operationID) != 64 || len(queryHash) != 64 || len(responseHash) != 64 {
		return Reservation{}, errors.New("databento reservation identity malformed")
	}
	if !slices.Contains(allowedBillableEndpoints, endpoint) {
		return Reservation{}, errors.New("databento reservation endpoint unsupported")
	}
	if costNano < 0 {
		return Reservation{}, errors.New("databento cost must be nonnegative")
	}
	if at.IsZero() || at.After(time.Now().Add(2*time.Minute)) {
		return Reservation{}, errors.New("databento reservation time invalid")
	}
	reserved, err := ReserveAmount(costNano)
	if err != nil {
		return Reservation{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lockFile == nil || l.failed {
		return Reservation{}, errors.New("databento ledger closed or requires recovery")
	}
	for _, r := range l.state.Reservations {
		if r.OperationID == operationID {
			if r.QueryHash != queryHash || r.Endpoint != endpoint || r.CostNano != costNano || r.ResponseHash != responseHash {
				return Reservation{}, errors.New("databento query changed after quote")
			}
			return Reservation{}, errors.New("databento operation already reserved; retry prohibited")
		}
	}
	capped := l.state.Version == ledgerVersion && l.state.LifetimeNano == MaxLifetimeNano && l.state.AuthorizationMode == ""
	uncapped := l.state.Version == uncappedLedgerVersion && l.state.LifetimeNano == uncappedLifetimeNano && l.state.AuthorizationMode == uncappedAuthorizationMode
	if !capped && !uncapped {
		return Reservation{}, errors.New("databento ledger authorization unsupported")
	}
	if endpoint == BatchSubmitEndpoint && !uncapped {
		return Reservation{}, errors.New("databento batch requires uncapped ledger")
	}
	var total int64
	for _, r := range l.state.Reservations {
		if r.ReservedNano < 0 {
			return Reservation{}, errors.New("databento ledger total exceeds authorization")
		}
		if capped {
			if r.ReservedNano > MaxLifetimeNano {
				return Reservation{}, errors.New("databento reservation exceeds authorization")
			}
			if total > MaxLifetimeNano-r.ReservedNano {
				return Reservation{}, errors.New("databento ledger total exceeds authorization")
			}
		} else if total > maxInt64Headroom-r.ReservedNano {
			return Reservation{}, errors.New("databento ledger total overflow")
		}
		total += r.ReservedNano
	}
	if capped {
		if total < 0 || total > MaxLifetimeNano {
			return Reservation{}, errors.New("databento ledger total exceeds authorization")
		}
		if reserved > MaxLifetimeNano {
			return Reservation{}, errors.New("databento cost exceeds lifetime authorization")
		}
		if reserved > MaxLifetimeNano-total {
			return Reservation{}, errors.New("databento cost exceeds remaining authorization")
		}
	} else {
		if total < 0 {
			return Reservation{}, errors.New("databento ledger total overflow")
		}
		if reserved > maxInt64Headroom-total {
			return Reservation{}, errors.New("databento reservation overflow")
		}
	}
	next := Reservation{
		OperationID:  strings.Clone(operationID),
		QueryHash:    strings.Clone(queryHash),
		Endpoint:     strings.Clone(endpoint),
		CostNano:     costNano,
		ReservedNano: reserved,
		At:           at.UTC(),
		ResponseHash: strings.Clone(responseHash),
		Status:       "reserved",
	}
	if err := validateReservation(next); err != nil {
		return Reservation{}, err
	}
	if capped && next.ReservedNano > MaxLifetimeNano {
		return Reservation{}, errors.New("databento reservation exceeds authorization")
	}
	updated := ledgerState{
		Version:           l.state.Version,
		LifetimeNano:      l.state.LifetimeNano,
		AuthorizationMode: l.state.AuthorizationMode,
		Reservations:      append(slices.Clone(l.state.Reservations), next),
	}
	if err := validateLedgerState(updated); err != nil {
		return Reservation{}, err
	}
	if err := writeLedgerFile(l.path, updated); err != nil {
		l.failed = true
		return Reservation{}, err
	}
	l.state = updated
	return next, nil
}

// Complete durably records the received body without releasing its reservation.
// Failure poisons this handle: reopen and inspect the on-disk evidence.
func (l *Ledger) Complete(operationID, dataSHA256 string, count int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lockFile == nil || l.failed {
		return errors.New("databento ledger closed or requires recovery")
	}
	updated := l.state
	updated.Reservations = slices.Clone(l.state.Reservations)
	for i := range updated.Reservations {
		r := &updated.Reservations[i]
		if r.OperationID != operationID {
			continue
		}
		if r.Status != "reserved" {
			return errors.New("databento receipt already completed")
		}
		if r.Endpoint == BatchSubmitEndpoint {
			return errors.New("databento batch acknowledgement is not complete")
		}
		r.Status, r.DataSHA256, r.Bytes = "complete", dataSHA256, count
		if err := validateReservation(*r); err != nil {
			return err
		}
		if err := writeLedgerFile(l.path, updated); err != nil {
			l.failed = true
			return err
		}
		l.state = updated
		return nil
	}
	return errors.New("databento reservation not found")
}

func writeLedgerFile(path string, st ledgerState) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return errors.New("databento ledger encode failed")
	}
	if !bytes.HasSuffix(raw, []byte{'\n'}) {
		raw = append(raw, '\n')
	}
	capacity := len(raw)
	for _, reservation := range st.Reservations {
		if reservation.Status == "reserved" {
			capacity += 128
		}
	}
	if capacity > maxLedgerFileBytes {
		return errors.New("databento ledger evidence capacity exceeded")
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".ledger-*")
	if err != nil {
		return errors.New("databento ledger write failed")
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return errors.New("databento ledger write failed")
	}
	var werr error
	if _, werr = tmp.Write(raw); werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); cerr != nil && werr == nil {
		werr = cerr
	}
	if werr != nil {
		return errors.New("databento ledger write failed")
	}
	if err := os.Rename(tmpName, path); err != nil {
		return errors.New("databento ledger write failed")
	}
	d, err := os.Open(dir)
	if err != nil {
		return errors.New("databento ledger write failed")
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return errors.New("databento ledger write failed")
	}
	return nil
}

// Close releases the ledger lock.
func (l *Ledger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lockFile == nil {
		return nil
	}
	fd := int(l.lockFile.Fd())
	unix.Flock(fd, unix.LOCK_UN)
	err := l.lockFile.Close()
	l.lockFile = nil
	return err
}
