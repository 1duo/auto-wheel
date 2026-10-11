// Package coordinator owns the account's durable permissions and reservations.
// It never calls the broker: committed state must precede financial transport.
package coordinator

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

const MaxCapital int64 = 8_000_000
const maxMoney int64 = 100_000_000_000_000

type Registration struct {
	Strategy   string `json:"strategy"`
	Release    string `json:"release"`
	PolicyHash string `json:"policy_hash"`
	NewRisk    bool   `json:"new_risk"`
	Management bool   `json:"management"`
	Epoch      int64  `json:"epoch"`
}
type Ownership struct {
	UnderlyingID        string `json:"underlying_id"`
	Symbol              string `json:"symbol"`
	Strategy            string `json:"strategy"`
	QuantityMicros      int64  `json:"quantity_micros"`
	CostCents           int64  `json:"cost_cents"`
	PutCollateralCents  int64  `json:"put_collateral_cents"`
	CashHoldCents       int64  `json:"cash_hold_cents"`
	CoveredSharesMicros int64  `json:"covered_shares_micros"`
	SettlementHold      bool   `json:"settlement_hold"`
	Version             int64  `json:"version"`
}
type Import struct {
	AccountHash   string            `json:"account_identity_hash"`
	PolicyHash    string            `json:"policy_hash"`
	SourceProof   string            `json:"source_proof"`
	FreeCashCents int64             `json:"free_cash_cents"`
	CapitalCents  int64             `json:"capital_cents"`
	Owners        []Ownership       `json:"owners"`
	Registrations []Registration    `json:"registrations"`
	History       []HistoricalOrder `json:"historical_orders"`
}

// HistoricalOrder is an immutable, terminal baseline captured by the frozen
// cutover proof. It cannot turn an untracked new order into owned inventory.
type HistoricalOrder struct {
	ID       string          `json:"id"`
	Kind     string          `json:"kind"`
	Strategy string          `json:"strategy,omitempty"`
	Digest   string          `json:"digest"`
	Raw      json.RawMessage `json:"raw"`
}
type Request struct {
	IntentKey       string          `json:"intent_key"`
	UnderlyingID    string          `json:"underlying_id"`
	Symbol          string          `json:"symbol"`
	Kind            string          `json:"kind"`
	CashCents       int64           `json:"cash_cents"`
	CapitalCents    int64           `json:"capital_cents"`
	SharesMicros    int64           `json:"shares_micros"`
	QuantityMicros  int64           `json:"quantity_micros"`
	RefID           string          `json:"ref_id"`
	Digest          string          `json:"digest"`
	Payload         json.RawMessage `json:"payload"`
	OriginalOrderID string          `json:"original_order_id,omitempty"`
}
type Intent struct {
	Strategy      string          `json:"strategy"`
	Request       Request         `json:"request"`
	State         string          `json:"state"`
	TicketVersion int64           `json:"ticket_version"`
	Epoch         int64           `json:"permission_epoch"`
	OwnerVersion  int64           `json:"ownership_version"`
	ReviewDigest  string          `json:"review_digest"`
	ReviewID      string          `json:"review_id"`
	Review        json.RawMessage `json:"review"`
	ExpiresAt     time.Time       `json:"expires_at"`
	BrokerOrderID string          `json:"broker_order_id"`
	Response      json.RawMessage `json:"response"`
	FilledMicros  int64           `json:"filled_micros"`
	DebitCents    int64           `json:"debit_cents"`
}
type Status struct {
	Initialized   bool              `json:"initialized"`
	AccountHash   string            `json:"account_identity_hash"`
	PolicyHash    string            `json:"policy_hash"`
	SourceProof   string            `json:"source_proof"`
	Generation    int64             `json:"generation"`
	FreeCashCents int64             `json:"free_cash_cents"`
	CapitalCents  int64             `json:"committed_capital_cents"`
	AccountPaused bool              `json:"account_paused"`
	BlockedReason string            `json:"blocked_reason"`
	Owners        []Ownership       `json:"owners"`
	Intents       []Intent          `json:"intents"`
	Registrations []Registration    `json:"registrations"`
	History       []HistoricalOrder `json:"historical_orders"`
}

type Ledger struct {
	mu   sync.Mutex
	db   *sql.DB
	lock *os.File
}

func privateFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return errors.New("ledger file must be private and regular")
	}
	return nil
}

func OpenLedger(dir string) (*Ledger, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("ledger directory must be private and not a symlink")
	}
	for _, name := range []string{"ledger.sqlite3", "ledger.sqlite3-journal", "lock"} {
		if err := privateFile(filepath.Join(dir, name)); err != nil {
			return nil, err
		}
	}
	fd, err := syscall.Open(filepath.Join(dir, "lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	lock := os.NewFile(uintptr(fd), "account ledger lock")
	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("another process owns the account ledger")
	}
	path := filepath.Join(dir, "ledger.sqlite3")
	// Create privately before SQLite opens it; no public database creation window.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		lock.Close()
		return nil, err
	}
	f.Close()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(DELETE)&_pragma=synchronous(EXTRA)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)")
	if err != nil {
		lock.Close()
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	l := &Ledger{db: db, lock: lock}
	var mode string
	var durability, foreign int
	if err = db.QueryRow("PRAGMA journal_mode").Scan(&mode); err == nil {
		err = db.QueryRow("PRAGMA synchronous").Scan(&durability)
	}
	if err == nil {
		err = db.QueryRow("PRAGMA foreign_keys").Scan(&foreign)
	}
	if err != nil || mode != "delete" || durability != 3 || foreign != 1 {
		l.Close()
		return nil, errors.New("SQLite account durability settings unverified")
	}
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version > 1 {
		l.Close()
		return nil, errors.New("unsupported account ledger schema")
	}
	_, err = db.Exec(`
 CREATE TABLE IF NOT EXISTS account(id INTEGER PRIMARY KEY CHECK(id=1), account_hash TEXT NOT NULL, policy_hash TEXT NOT NULL, source_proof TEXT NOT NULL, generation INTEGER NOT NULL, free_cash INTEGER NOT NULL CHECK(free_cash>=0), capital INTEGER NOT NULL CHECK(capital>=0 AND capital<=8000000), paused INTEGER NOT NULL, blocked TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS registrations(strategy TEXT PRIMARY KEY CHECK(strategy IN ('tradepulse','autowheel')), release TEXT NOT NULL, policy_hash TEXT NOT NULL, new_risk INTEGER NOT NULL, management INTEGER NOT NULL, epoch INTEGER NOT NULL CHECK(epoch>0));
 CREATE TABLE IF NOT EXISTS owners(underlying TEXT PRIMARY KEY, symbol TEXT NOT NULL UNIQUE, strategy TEXT NOT NULL REFERENCES registrations(strategy), body TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS intents(strategy TEXT NOT NULL REFERENCES registrations(strategy), intent_key TEXT NOT NULL, ref TEXT NOT NULL UNIQUE, digest TEXT NOT NULL, broker_id TEXT UNIQUE, state TEXT NOT NULL, body TEXT NOT NULL, PRIMARY KEY(strategy,intent_key));
 CREATE TABLE IF NOT EXISTS events(event_id TEXT PRIMARY KEY, digest TEXT NOT NULL, body TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS history(kind TEXT NOT NULL,order_id TEXT NOT NULL,body TEXT NOT NULL,PRIMARY KEY(kind,order_id));
 PRAGMA user_version=1;
 `)
	if err != nil {
		l.Close()
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		l.Close()
		return nil, err
	}
	var integrity string
	if err = db.QueryRow("PRAGMA quick_check").Scan(&integrity); err != nil || integrity != "ok" {
		l.Close()
		return nil, errors.New("account ledger integrity check failed")
	}
	directory, err := os.Open(dir)
	if err != nil {
		l.Close()
		return nil, err
	}
	err = errors.Join(directory.Sync(), directory.Close())
	if err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}
func (l *Ledger) Close() error { return errors.Join(l.db.Close(), l.lock.Close()) }

func digestValid(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func validMoney(n int64) bool { return n >= 0 && n <= maxMoney }
func validOwner(o Ownership) bool {
	return o.UnderlyingID != "" && o.Symbol != "" && validMoney(o.QuantityMicros) && validMoney(o.CostCents) && validMoney(o.PutCollateralCents) && validMoney(o.CashHoldCents) && validMoney(o.CoveredSharesMicros) && o.CoveredSharesMicros <= o.QuantityMicros && o.CostCents <= MaxCapital-o.PutCollateralCents
}
func ownerCommitment(o Ownership) int64 { return o.CostCents + o.PutCollateralCents }
func (l *Ledger) Initialize(seed Import) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !digestValid(seed.AccountHash) || !digestValid(seed.PolicyHash) || !digestValid(seed.SourceProof) || !validMoney(seed.FreeCashCents) || seed.CapitalCents < 0 || seed.CapitalCents > MaxCapital || len(seed.Registrations) != 2 {
		return errors.New("invalid frozen account import")
	}
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err = tx.QueryRow("SELECT count(*) FROM account").Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return errors.New("account ledger already initialized")
	}
	for _, r := range seed.Registrations {
		if !digestValid(r.Release) || r.PolicyHash != seed.PolicyHash {
			return errors.New("invalid strategy release binding")
		}
		if _, err = tx.Exec("INSERT INTO registrations VALUES(?,?,?,?,?,1)", r.Strategy, r.Release, r.PolicyHash, r.NewRisk, r.Management); err != nil {
			return err
		}
	}
	var capital int64
	for _, o := range seed.Owners {
		if !validOwner(o) || o.Strategy != "tradepulse" || o.PutCollateralCents != 0 || o.CashHoldCents != 0 || o.CoveredSharesMicros != 0 || o.QuantityMicros <= 0 || o.CostCents <= 0 {
			return errors.New("import may adopt only proven TradePulse stock")
		}
		if capital > MaxCapital-ownerCommitment(o) {
			return errors.New("import capital exceeds ceiling")
		}
		capital += ownerCommitment(o)
		o.Version = 1
		raw, _ := json.Marshal(o)
		if _, err = tx.Exec("INSERT INTO owners VALUES(?,?,?,?)", o.UnderlyingID, o.Symbol, o.Strategy, string(raw)); err != nil {
			return err
		}
	}
	if capital != seed.CapitalCents {
		return errors.New("frozen stock costs disagree with capital")
	}
	for _, order := range seed.History {
		if order.ID == "" || order.Kind != "equity" && order.Kind != "option" && order.Kind != "crypto" || order.Strategy != "" && order.Strategy != "tradepulse" || !json.Valid(order.Raw) || hash(order.Raw) != order.Digest {
			return errors.New("historical order is not bound to the frozen proof")
		}
		var identity struct {
			ID    string `json:"id"`
			State string `json:"state"`
		}
		if json.Unmarshal(order.Raw, &identity) != nil || identity.ID != order.ID || !brokerTerminalState(identity.State) {
			return errors.New("frozen order baseline must be conclusively terminal")
		}
		raw, _ := json.Marshal(order)
		if _, err = tx.Exec("INSERT INTO history VALUES(?,?,?)", order.Kind, order.ID, string(raw)); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("INSERT INTO account VALUES(1,?,?,?,1,?,?,1,'')", seed.AccountHash, seed.PolicyHash, seed.SourceProof, seed.FreeCashCents, capital); err != nil {
		return err
	}
	return tx.Commit()
}

type querier interface {
	QueryRow(string, ...any) *sql.Row
	Query(string, ...any) (*sql.Rows, error)
}

func status(q querier) (Status, error) {
	var s Status
	err := q.QueryRow("SELECT account_hash,policy_hash,source_proof,generation,free_cash,capital,paused,blocked FROM account WHERE id=1").Scan(&s.AccountHash, &s.PolicyHash, &s.SourceProof, &s.Generation, &s.FreeCashCents, &s.CapitalCents, &s.AccountPaused, &s.BlockedReason)
	if errors.Is(err, sql.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	s.Initialized = true
	rows, err := q.Query("SELECT strategy,release,policy_hash,new_risk,management,epoch FROM registrations ORDER BY strategy")
	if err != nil {
		return s, err
	}
	for rows.Next() {
		var r Registration
		if err = rows.Scan(&r.Strategy, &r.Release, &r.PolicyHash, &r.NewRisk, &r.Management, &r.Epoch); err != nil {
			rows.Close()
			return s, err
		}
		s.Registrations = append(s.Registrations, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return s, err
	}
	for _, collection := range []struct {
		query string
		owner bool
	}{{"SELECT body FROM owners ORDER BY symbol", true}, {"SELECT body FROM intents ORDER BY strategy,intent_key", false}} {
		rows, err = q.Query(collection.query)
		if err != nil {
			return s, err
		}
		for rows.Next() {
			var raw string
			if err = rows.Scan(&raw); err != nil {
				rows.Close()
				return s, err
			}
			if collection.owner {
				var o Ownership
				if err = json.Unmarshal([]byte(raw), &o); err == nil {
					s.Owners = append(s.Owners, o)
				}
			} else {
				var in Intent
				if err = json.Unmarshal([]byte(raw), &in); err == nil {
					s.Intents = append(s.Intents, in)
				}
			}
			if err != nil {
				rows.Close()
				return s, err
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return s, err
		}
	}
	rows, err = q.Query("SELECT body FROM history ORDER BY kind,order_id")
	if err != nil {
		return s, err
	}
	for rows.Next() {
		var raw string
		var order HistoricalOrder
		if err = rows.Scan(&raw); err == nil {
			err = json.Unmarshal([]byte(raw), &order)
		}
		if err != nil {
			rows.Close()
			return s, err
		}
		s.History = append(s.History, order)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return s, err
	}
	return s, nil
}
func (l *Ledger) Status() (Status, error) { l.mu.Lock(); defer l.mu.Unlock(); return status(l.db) }
func lookup(q querier, strategy, key string) (Intent, error) {
	var in Intent
	var raw string
	err := q.QueryRow("SELECT body FROM intents WHERE strategy=? AND intent_key=?", strategy, key).Scan(&raw)
	if err != nil {
		return in, err
	}
	err = json.Unmarshal([]byte(raw), &in)
	return in, err
}
func (l *Ledger) Lookup(strategy, key string) (Intent, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return lookup(l.db, strategy, key)
}
func saveIntent(tx *sql.Tx, in Intent) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	var id any
	if in.BrokerOrderID != "" {
		id = in.BrokerOrderID
	}
	_, err = tx.Exec("UPDATE intents SET broker_id=?,state=?,body=? WHERE strategy=? AND intent_key=?", id, in.State, string(raw), in.Strategy, in.Request.IntentKey)
	return err
}
func saveOwner(tx *sql.Tx, o Ownership) error {
	raw, err := json.Marshal(o)
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO owners VALUES(?,?,?,?) ON CONFLICT(underlying) DO UPDATE SET body=excluded.body", o.UnderlyingID, o.Symbol, o.Strategy, string(raw))
	return err
}
func terminalState(s string) bool {
	return s == "withdrawn" || s == "terminal" || s == "cancel_terminal"
}
func brokerTerminalState(s string) bool {
	return s == "filled" || s == "cancelled" || s == "canceled" || s == "partially_filled_rest_cancelled" || s == "rejected" || s == "failed" || s == "voided"
}
func unknownState(s string) bool {
	return s == "send_started" || s == "uncertain" || s == "cancel_started"
}
func permission(s Status, strategy, kind string) (Registration, error) {
	for _, r := range s.Registrations {
		if r.Strategy == strategy {
			if s.AccountPaused || s.BlockedReason != "" {
				return r, errors.New("account is paused or unresolved")
			}
			if kind == "stock_sell" || kind == "cancel" {
				if !r.Management {
					return r, errors.New("management permission revoked")
				}
			} else if !r.NewRisk {
				return r, errors.New("new risk permission revoked")
			}
			return r, nil
		}
	}
	return Registration{}, errors.New("strategy not registered")
}

func (l *Ledger) Reserve(strategy string, r Request, brokerCash int64, now time.Time) (Intent, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var empty Intent
	if r.IntentKey == "" || r.UnderlyingID == "" || r.Symbol == "" || r.RefID == "" || !digestValid(r.Digest) || !validMoney(r.CashCents) || !validMoney(r.CapitalCents) || !validMoney(r.SharesMicros) || !validMoney(r.QuantityMicros) || !validMoney(brokerCash) || now.IsZero() {
		return empty, errors.New("invalid exact admission request")
	}
	if r.Kind != "stock_buy" && r.Kind != "stock_sell" && r.Kind != "put" && r.Kind != "call" && r.Kind != "cancel" {
		return empty, errors.New("unsupported coordinator order")
	}
	if strategy == "autowheel" && r.Kind != "put" && r.Kind != "call" || strategy == "tradepulse" && (r.Kind == "put" || r.Kind == "call") {
		return empty, errors.New("strategy financial capability denied")
	}
	if (r.Kind == "stock_buy" || r.Kind == "put") && (r.CashCents <= 0 || r.CapitalCents <= 0) || r.CapitalCents > r.CashCents {
		return empty, errors.New("buy or put debit bound missing")
	}
	tx, err := l.db.Begin()
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	if prior, err := lookup(tx, strategy, r.IntentKey); err == nil {
		old, _ := json.Marshal(prior.Request)
		fresh, _ := json.Marshal(r)
		if string(old) != string(fresh) {
			return empty, errors.New("intent key reused with different exact order")
		}
		return prior, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return empty, err
	}
	s, err := status(tx)
	if err != nil {
		return empty, err
	}
	if !s.Initialized {
		return empty, errors.New("frozen ownership import missing")
	}
	reg, err := permission(s, strategy, r.Kind)
	if err != nil {
		return empty, err
	}
	if r.Kind == "cancel" {
		var owningStrategy, originalState string
		if r.OriginalOrderID == "" || tx.QueryRow("SELECT strategy,state FROM intents WHERE broker_id=?", r.OriginalOrderID).Scan(&owningStrategy, &originalState) != nil || owningStrategy != strategy || terminalState(originalState) || r.CashCents != 0 || r.CapitalCents != 0 || r.SharesMicros != 0 {
			return empty, errors.New("cancellation is not bound to an active owned order")
		}
	}
	for _, in := range s.Intents {
		if unknownState(in.State) {
			return empty, errors.New("uncertain account transport retains all obligations")
		}
	}
	var owner Ownership
	var found bool
	var awCount int
	var heldShares int64
	for _, o := range s.Owners {
		if o.Strategy == "autowheel" {
			awCount++
		}
		if o.Symbol == r.Symbol && o.UnderlyingID != r.UnderlyingID {
			return empty, errors.New("symbol maps to a different underlying")
		}
		if o.UnderlyingID == r.UnderlyingID {
			owner = o
			found = true
		}
	}
	if found && (owner.Strategy != strategy || owner.Symbol != r.Symbol) {
		return empty, errors.New("underlying owned by peer")
	}
	if !found {
		if strategy == "autowheel" && awCount >= 3 {
			return empty, errors.New("AutoWheel's three underlying slots are occupied")
		}
		if r.Kind != "stock_buy" && r.Kind != "put" {
			return empty, errors.New("owned inventory missing")
		}
		owner = Ownership{UnderlyingID: r.UnderlyingID, Symbol: r.Symbol, Strategy: strategy, Version: 1}
	}
	if owner.SettlementHold {
		return empty, errors.New("underlying settlement is unresolved")
	}
	for _, in := range s.Intents {
		if in.Request.UnderlyingID == r.UnderlyingID && !terminalState(in.State) {
			if in.Request.Kind == "put" || in.Request.Kind == "call" || r.Kind == "put" || r.Kind == "call" {
				return empty, errors.New("underlying already has option obligations")
			}
			if heldShares > maxMoney-in.Request.SharesMicros {
				return empty, errors.New("share holds overflow")
			}
			heldShares += in.Request.SharesMicros
		}
	}
	if r.Kind == "put" && (owner.QuantityMicros != 0 || owner.PutCollateralCents != 0 || owner.CoveredSharesMicros != 0) {
		return empty, errors.New("put underlying is not flat")
	}
	if r.Kind == "call" && (r.SharesMicros != 100_000_000 || owner.QuantityMicros != 100_000_000 || owner.PutCollateralCents != 0 || owner.CoveredSharesMicros != 0) {
		return empty, errors.New("call requires exactly 100 owned uncovered shares")
	}
	if r.SharesMicros > owner.QuantityMicros-owner.CoveredSharesMicros-heldShares {
		return empty, errors.New("owned available shares insufficient or reserved for call")
	}
	if r.CashCents > min(s.FreeCashCents, brokerCash) || r.CapitalCents > MaxCapital-s.CapitalCents {
		return empty, errors.New("shared cash or capital ceiling insufficient")
	}
	if err = saveOwner(tx, owner); err != nil {
		return empty, err
	}
	in := Intent{Strategy: strategy, Request: r, State: "reserved", Epoch: reg.Epoch, OwnerVersion: owner.Version}
	raw, _ := json.Marshal(in)
	if _, err = tx.Exec("INSERT INTO intents VALUES(?,?,?,?,NULL,?,?)", strategy, r.IntentKey, r.RefID, r.Digest, in.State, string(raw)); err != nil {
		return empty, err
	}
	if _, err = tx.Exec("UPDATE account SET free_cash=free_cash-?,capital=capital+?,generation=generation+1 WHERE id=1", r.CashCents, r.CapitalCents); err != nil {
		return empty, err
	}
	if err = tx.Commit(); err != nil {
		return empty, err
	}
	return in, nil
}

func (l *Ledger) ReviewPrepared(strategy, key, reviewDigest, reviewID string, review json.RawMessage, expires time.Time) (Intent, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var empty Intent
	if !digestValid(reviewDigest) || !json.Valid(review) || expires.IsZero() || hash(review) != reviewDigest {
		return empty, errors.New("durable review evidence missing")
	}
	tx, err := l.db.Begin()
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	in, err := lookup(tx, strategy, key)
	if err != nil {
		return empty, err
	}
	if in.State != "reserved" && in.State != "prepared" {
		return empty, errors.New("review cannot refresh a started or terminal intent")
	}
	s, err := status(tx)
	if err != nil {
		return empty, err
	}
	reg, err := permission(s, strategy, in.Request.Kind)
	if err != nil {
		return empty, err
	}
	// A permission change requires a fresh review and ticket; a stale ticket never
	// gains a renewed epoch implicitly in StartSend.
	in.State = "prepared"
	in.TicketVersion++
	in.Epoch = reg.Epoch
	in.ReviewDigest = reviewDigest
	in.ReviewID = reviewID
	in.Review = append(json.RawMessage(nil), review...)
	in.ExpiresAt = expires
	if err = saveIntent(tx, in); err != nil {
		return empty, err
	}
	if err = tx.Commit(); err != nil {
		return empty, err
	}
	return in, nil
}

func (l *Ledger) StartSend(strategy, key string, ticket int64, digest string, now time.Time) (Intent, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var empty Intent
	tx, err := l.db.Begin()
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	in, err := lookup(tx, strategy, key)
	if err != nil {
		return empty, err
	}
	if in.State != "prepared" || in.TicketVersion != ticket || in.Request.Digest != digest || !digestValid(in.ReviewDigest) || len(in.Review) == 0 || !now.Before(in.ExpiresAt) {
		return empty, errors.New("coordinator ticket missing, stale, superseded or already started")
	}
	s, err := status(tx)
	if err != nil {
		return empty, err
	}
	reg, err := permission(s, strategy, in.Request.Kind)
	if err != nil {
		return empty, err
	}
	if reg.Epoch != in.Epoch {
		return empty, errors.New("permission epoch changed after review")
	}
	for _, other := range s.Intents {
		if unknownState(other.State) {
			return empty, errors.New("uncertain account transport blocks a second send")
		}
	}
	found := false
	for _, o := range s.Owners {
		if o.UnderlyingID == in.Request.UnderlyingID && o.Strategy == strategy && o.Version == in.OwnerVersion && !o.SettlementHold {
			found = true
		}
	}
	if !found {
		return empty, errors.New("reviewed ownership changed")
	}
	in.State = "send_started"
	if in.Request.Kind == "cancel" {
		in.State = "cancel_started"
	}
	if err = saveIntent(tx, in); err != nil {
		return empty, err
	}
	if err = tx.Commit(); err != nil {
		return empty, err
	}
	return in, nil
}
func (l *Ledger) Complete(strategy, key, id string, response json.RawMessage) error {
	if id == "" || !json.Valid(response) {
		return errors.New("authoritative broker receipt missing")
	}
	return l.updateIntent(strategy, key, func(in *Intent) error {
		if in.State != "send_started" {
			return errors.New("placement was not started exactly once")
		}
		in.State = "accepted"
		in.BrokerOrderID = id
		in.Response = append(json.RawMessage(nil), response...)
		return nil
	})
}
func (l *Ledger) MarkUncertain(strategy, key string) error {
	return l.updateIntent(strategy, key, func(in *Intent) error {
		if in.State != "send_started" && in.State != "cancel_started" && in.State != "uncertain" {
			return errors.New("unstarted intent cannot become uncertain")
		}
		in.State = "uncertain"
		return nil
	})
}
func (l *Ledger) updateIntent(strategy, key string, update func(*Intent) error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	in, err := lookup(tx, strategy, key)
	if err != nil {
		return err
	}
	if err = update(&in); err != nil {
		return err
	}
	if err = saveIntent(tx, in); err != nil {
		return err
	}
	return tx.Commit()
}

func (l *Ledger) Withdraw(strategy, key string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	in, err := lookup(tx, strategy, key)
	if err != nil {
		return err
	}
	if in.State == "withdrawn" {
		return nil
	}
	if in.State != "reserved" && in.State != "prepared" {
		return errors.New("transport may have started; resources retained")
	}
	in.State = "withdrawn"
	in.TicketVersion++
	if err = saveIntent(tx, in); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE account SET free_cash=free_cash+?,capital=capital-?,generation=generation+1 WHERE id=1", in.Request.CashCents, in.Request.CapitalCents); err != nil {
		return err
	}
	s, err := status(tx)
	if err != nil {
		return err
	}
	for _, o := range s.Owners {
		if o.UnderlyingID == in.Request.UnderlyingID && o.QuantityMicros == 0 && ownerCommitment(o) == 0 && o.CashHoldCents == 0 && o.CoveredSharesMicros == 0 && !o.SettlementHold {
			held := false
			for _, other := range s.Intents {
				if other.Request.UnderlyingID == o.UnderlyingID && !terminalState(other.State) {
					held = true
				}
			}
			if !held {
				if _, err = tx.Exec("DELETE FROM owners WHERE underlying=?", o.UnderlyingID); err != nil {
					return err
				}
			}
		}
	}
	return tx.Commit()
}
func (l *Ledger) SetPermission(strategy string, newRisk, management bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, err := l.db.Exec("UPDATE registrations SET new_risk=?,management=?,epoch=epoch+1 WHERE strategy=?", newRisk, management, strategy)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("strategy not registered")
	}
	return nil
}
func (l *Ledger) SetAccountPause(paused bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE account SET paused=?,generation=generation+1 WHERE id=1", paused); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE registrations SET epoch=epoch+1"); err != nil {
		return err
	}
	return tx.Commit()
}
func (l *Ledger) SetBlocked(reason string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE account SET blocked=?,generation=generation+1 WHERE id=1", reason); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE registrations SET epoch=epoch+1"); err != nil {
		return err
	}
	return tx.Commit()
}

// TerminalProof must be derived by the server from exact bound broker facts,
// never accepted from a strategy RPC. Unknown fees/settlement retain CashHold.
type TerminalProof struct {
	BrokerOrderID  string    `json:"broker_order_id"`
	EvidenceDigest string    `json:"evidence_digest"`
	FilledMicros   int64     `json:"filled_micros"`
	DebitCents     int64     `json:"debit_cents"`
	Ownership      Ownership `json:"ownership"`
}

func (l *Ledger) ApplyTerminal(strategy, key string, p TerminalProof) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !digestValid(p.EvidenceDigest) || !validMoney(p.FilledMicros) || !validMoney(p.DebitCents) || !validOwner(p.Ownership) {
		return errors.New("terminal account proof incomplete")
	}
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	in, err := lookup(tx, strategy, key)
	if err != nil {
		return err
	}
	if in.State == "terminal" {
		if in.FilledMicros == p.FilledMicros && in.DebitCents == p.DebitCents && in.BrokerOrderID == p.BrokerOrderID {
			return nil
		}
		return errors.New("terminal cumulative fill changed")
	}
	if in.State != "accepted" || in.BrokerOrderID != p.BrokerOrderID || p.FilledMicros > in.Request.QuantityMicros || p.DebitCents > in.Request.CashCents {
		return errors.New("terminal fill is not bound to the accepted order")
	}
	s, err := status(tx)
	if err != nil {
		return err
	}
	var old Ownership
	found := false
	for _, o := range s.Owners {
		if o.UnderlyingID == in.Request.UnderlyingID {
			old = o
			found = true
		}
	}
	next := p.Ownership
	if !found || next.UnderlyingID != old.UnderlyingID || next.Symbol != old.Symbol || next.Strategy != strategy {
		return errors.New("terminal ownership identity mismatch")
	}
	if (in.Request.Kind == "stock_buy" && next.QuantityMicros != old.QuantityMicros+p.FilledMicros) || (in.Request.Kind == "stock_sell" && (p.FilledMicros > old.QuantityMicros-old.CoveredSharesMicros || next.QuantityMicros != old.QuantityMicros-p.FilledMicros)) {
		return errors.New("cumulative fill does not explain inventory")
	}
	switch in.Request.Kind {
	case "stock_buy":
		if p.FilledMicros > 0 && p.DebitCents <= 0 || p.FilledMicros == 0 && p.DebitCents != 0 || next.CostCents != old.CostCents+p.DebitCents || next.PutCollateralCents != old.PutCollateralCents || next.CoveredSharesMicros != old.CoveredSharesMicros || next.CashHoldCents != old.CashHoldCents {
			return errors.New("stock buy cannot understate committed debit or change other obligations")
		}
	case "stock_sell":
		cost, err := RemainingCost(old.CostCents, old.QuantityMicros, next.QuantityMicros)
		if err != nil || next.CostCents != cost || next.PutCollateralCents != old.PutCollateralCents || next.CoveredSharesMicros != old.CoveredSharesMicros || next.CashHoldCents != old.CashHoldCents {
			return errors.New("stock sale must retain the exact conservative remaining capital")
		}
	case "put", "call":
		if next.QuantityMicros != old.QuantityMicros || next.CostCents != old.CostCents || p.FilledMicros != 0 && p.FilledMicros != 1_000_000 || next.CashHoldCents < old.CashHoldCents {
			return errors.New("option fill cannot transfer stock or release unrelated cash")
		}
		if p.FilledMicros == 0 && (next.PutCollateralCents != old.PutCollateralCents || next.CoveredSharesMicros != old.CoveredSharesMicros) || in.Request.Kind == "put" && next.CoveredSharesMicros != old.CoveredSharesMicros || in.Request.Kind == "call" && next.PutCollateralCents != old.PutCollateralCents {
			return errors.New("option terminal proof changed unrelated coverage or collateral")
		}
	}
	if in.Request.Kind == "call" && p.FilledMicros > 0 && next.CoveredSharesMicros != 100_000_000 {
		return errors.New("filled call must retain its owned coverage")
	}
	if in.Request.Kind == "put" && p.FilledMicros > 0 && (next.PutCollateralCents != in.Request.CapitalCents || next.CashHoldCents < next.PutCollateralCents) {
		return errors.New("filled put must retain full principal collateral")
	}
	refund := in.Request.CashCents + old.CashHoldCents - p.DebitCents - next.CashHoldCents
	capital := s.CapitalCents - in.Request.CapitalCents - ownerCommitment(old) + ownerCommitment(next)
	if refund < 0 || s.FreeCashCents > maxMoney-refund || capital < 0 || capital > MaxCapital {
		return errors.New("terminal resources exceed reserved debit or capital")
	}
	next.Version = old.Version + 1
	in.State = "terminal"
	in.FilledMicros = p.FilledMicros
	in.DebitCents = p.DebitCents
	if err = saveOwner(tx, next); err != nil {
		return err
	}
	if err = saveIntent(tx, in); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE account SET free_cash=free_cash+?,capital=?,generation=generation+1 WHERE id=1", refund, capital); err != nil {
		return err
	}
	raw, _ := json.Marshal(p)
	if _, err = tx.Exec("INSERT INTO events VALUES(?,?,?)", strategy+":"+key+":terminal", p.EvidenceDigest, string(raw)); err != nil {
		return err
	}
	return tx.Commit()
}

// RemainingCost retains rounded-up capital after a proportional stock sale.
// Acquisition fees remain part of committed capital. A checked big integer
// intermediate avoids overflow in exact microshare products.
func RemainingCost(cost, prior, remaining int64) (int64, error) {
	if !validMoney(cost) || prior <= 0 || !validMoney(prior) || remaining < 0 || remaining > prior {
		return 0, errors.New("remaining stock capital proof invalid")
	}
	n := new(big.Int).Mul(big.NewInt(cost), big.NewInt(remaining))
	q, r := new(big.Int), new(big.Int)
	q.QuoRem(n, big.NewInt(prior), r)
	if r.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() {
		return 0, errors.New("remaining capital overflows")
	}
	return q.Int64(), nil
}

// ApplySettledCredit records an exact settlement event once. Reconciled broker
// buying power is an alternative bound, not another subtraction of reservations.
func (l *Ledger) ApplySettledCredit(id, digest string, cents, brokerCash int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if id == "" || !digestValid(digest) || !validMoney(cents) || !validMoney(brokerCash) {
		return errors.New("settled credit proof missing")
	}
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var prior string
	err = tx.QueryRow("SELECT digest FROM events WHERE event_id=?", id).Scan(&prior)
	if err == nil {
		if prior != digest {
			return errors.New("settled event proof changed")
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	s, err := status(tx)
	if err != nil {
		return err
	}
	if !s.Initialized || s.FreeCashCents > maxMoney-cents {
		return errors.New("settled credit exceeds checked cash range")
	}
	raw, _ := json.Marshal(map[string]int64{"credit_cents": cents, "broker_cash_cents": brokerCash})
	if _, err = tx.Exec("INSERT INTO events VALUES(?,?,?)", id, digest, string(raw)); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE account SET free_cash=?,generation=generation+1 WHERE id=1", min(s.FreeCashCents+cents, brokerCash)); err != nil {
		return err
	}
	return tx.Commit()
}

func hash(raw []byte) string {
	const digits = "0123456789abcdef"
	h := sha256.Sum256(raw)
	out := make([]byte, 64)
	for i, b := range h {
		out[2*i], out[2*i+1] = digits[b>>4], digits[b&15]
	}
	return string(out)
}

func (l *Ledger) CompleteCancel(strategy, key string, response json.RawMessage) error {
	if !json.Valid(response) {
		return errors.New("cancel response missing")
	}
	return l.updateIntent(strategy, key, func(in *Intent) error {
		if in.State != "cancel_started" {
			return errors.New("cancellation was not started once")
		}
		in.State = "cancel_pending"
		in.Response = append(json.RawMessage(nil), response...)
		return nil
	})
}

func (l *Ledger) ConfirmCancelTerminal(strategy, key, originalID, proof string) error {
	if !digestValid(proof) {
		return errors.New("exact terminal cancellation proof missing")
	}
	return l.updateIntent(strategy, key, func(in *Intent) error {
		if in.State == "cancel_terminal" {
			return nil
		}
		if in.State != "cancel_pending" || in.Request.OriginalOrderID != originalID {
			return errors.New("cancel acknowledgment is not terminal proof")
		}
		in.State = "cancel_terminal"
		return nil
	})
}

// InventoryTransition records a broker-qualified expiration/assignment. The
// actor must first prove the contract disappeared and settlement checks passed.
type InventoryTransition struct {
	EventID        string    `json:"event_id"`
	EvidenceDigest string    `json:"evidence_digest"`
	Kind           string    `json:"kind"`
	PriorVersion   int64     `json:"prior_version"`
	DebitCents     int64     `json:"debit_cents"`
	Ownership      Ownership `json:"ownership"`
}

func (l *Ledger) ApplyInventoryTransition(p InventoryTransition) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if p.EventID == "" || !digestValid(p.EvidenceDigest) || !validMoney(p.DebitCents) || !validOwner(p.Ownership) || p.Ownership.SettlementHold {
		return errors.New("settled inventory proof incomplete")
	}
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var recorded string
	err = tx.QueryRow("SELECT digest FROM events WHERE event_id=?", p.EventID).Scan(&recorded)
	if err == nil {
		if recorded != p.EvidenceDigest {
			return errors.New("inventory event proof changed")
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	s, err := status(tx)
	if err != nil {
		return err
	}
	var old Ownership
	found := false
	for _, o := range s.Owners {
		if o.UnderlyingID == p.Ownership.UnderlyingID {
			old = o
			found = true
		}
	}
	next := p.Ownership
	if !found || old.Version != p.PriorVersion || old.Strategy != "autowheel" || next.Strategy != old.Strategy || next.Symbol != old.Symbol {
		return errors.New("inventory transition ownership mismatch")
	}
	for _, in := range s.Intents {
		if in.Request.UnderlyingID == old.UnderlyingID && !terminalState(in.State) {
			return errors.New("inventory transition has unresolved orders")
		}
	}
	switch p.Kind {
	case "put_assignment":
		if old.PutCollateralCents <= 0 || old.QuantityMicros != 0 || next.QuantityMicros != 100_000_000 || next.CostCents != old.PutCollateralCents || next.PutCollateralCents != 0 || next.CoveredSharesMicros != 0 || p.DebitCents < old.PutCollateralCents {
			return errors.New("assignment must convert one full put into 100 owned shares")
		}
	case "put_expiry":
		if old.PutCollateralCents <= 0 || next.QuantityMicros != 0 || ownerCommitment(next) != 0 || next.CoveredSharesMicros != 0 || p.DebitCents > old.CashHoldCents-old.PutCollateralCents {
			return errors.New("put expiry cannot consume or transfer principal")
		}
	case "call_away":
		if old.QuantityMicros != 100_000_000 || old.CoveredSharesMicros != 100_000_000 || next.QuantityMicros != 0 || ownerCommitment(next) != 0 || next.CoveredSharesMicros != 0 {
			return errors.New("call-away must reconcile exact owned coverage")
		}
	case "call_expiry":
		if old.CoveredSharesMicros != 100_000_000 || next.QuantityMicros != old.QuantityMicros || next.CostCents != old.CostCents || next.PutCollateralCents != 0 || next.CoveredSharesMicros != 0 {
			return errors.New("call expiry must retain owned stock")
		}
	default:
		return errors.New("unsupported inventory event")
	}
	refund := old.CashHoldCents - p.DebitCents - next.CashHoldCents
	capital := s.CapitalCents - ownerCommitment(old) + ownerCommitment(next)
	if refund < 0 || s.FreeCashCents > maxMoney-refund || capital < 0 || capital > MaxCapital {
		return errors.New("inventory event exceeds retained resources")
	}
	next.Version = old.Version + 1
	if err = saveOwner(tx, next); err != nil {
		return err
	}
	raw, _ := json.Marshal(p)
	if _, err = tx.Exec("INSERT INTO events VALUES(?,?,?)", p.EventID, p.EvidenceDigest, string(raw)); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE account SET free_cash=free_cash+?,capital=?,generation=generation+1 WHERE id=1", refund, capital); err != nil {
		return err
	}
	return tx.Commit()
}
func (l *Ledger) ReleaseFlatOwner(underlying, strategy string, version int64, proof string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !digestValid(proof) {
		return errors.New("flat account proof missing")
	}
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	s, err := status(tx)
	if err != nil {
		return err
	}
	var found bool
	for _, o := range s.Owners {
		if o.UnderlyingID == underlying {
			found = true
			if o.Strategy != strategy || o.Version != version || o.QuantityMicros != 0 || ownerCommitment(o) != 0 || o.CashHoldCents != 0 || o.CoveredSharesMicros != 0 || o.SettlementHold {
				return errors.New("ownership is not conclusively flat")
			}
		}
	}
	if !found {
		return nil
	}
	for _, in := range s.Intents {
		if in.Request.UnderlyingID == underlying && !terminalState(in.State) {
			return errors.New("flat claim has unresolved order obligations")
		}
	}
	if _, err = tx.Exec("DELETE FROM owners WHERE underlying=?", underlying); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE account SET generation=generation+1 WHERE id=1"); err != nil {
		return err
	}
	return tx.Commit()
}
