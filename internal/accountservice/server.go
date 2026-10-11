// Package accountservice serializes financial writes through one durable ledger.
package accountservice

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"autowheel/internal/coordinator"
	"autowheel/internal/robinhood"
	aruntime "autowheel/internal/runtime"
	"autowheel/internal/sharedbroker"
	"autowheel/internal/sharedclient"
	"autowheel/internal/wheel"
)

type Config struct {
	SocketDir            string
	PolicyHash           string
	Releases             map[string]string
	PendingQualification []string
	// Qualification is supplied only by a guarded production cutover. An
	// observation service cannot turn this on through its local strategy API.
	Qualification func(coordinator.Status, sharedbroker.Facts) error
}
type Broker interface {
	Snapshot(context.Context) (sharedbroker.Facts, error)
	Underlying(context.Context, string) (string, error)
	Plan(context.Context, string, json.RawMessage) (coordinator.Request, error)
	Review(context.Context, coordinator.Intent) (json.RawMessage, time.Time, error)
	PlaceOnce(context.Context, coordinator.Intent) (string, json.RawMessage, error)
	CancelOnce(context.Context, coordinator.Intent) (json.RawMessage, error)
}
type job struct {
	strategy string
	request  sharedclient.Request
	reply    chan sharedclient.Response
	ctx      context.Context
}
type Server struct {
	Config     Config
	Ledger     *coordinator.Ledger
	Broker     Broker
	mu         sync.RWMutex
	latest     sharedbroker.Facts
	lastError  string
	entries    chan job
	management chan job
}

func (s *Server) Run(ctx context.Context) error {
	if s.Config.Qualification == nil {
		s.Config.Qualification = func(coordinator.Status, sharedbroker.Facts) error {
			return errors.New("shared cutover and execution policy have not been qualified")
		}
	}
	if err := os.MkdirAll(s.Config.SocketDir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(s.Config.SocketDir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("coordinator sockets require a private directory")
	}
	s.entries = make(chan job, 32)
	s.management = make(chan job, 32)
	var listeners []net.Listener
	defer func() {
		for _, listener := range listeners {
			listener.Close()
		}
	}()
	var wg sync.WaitGroup
	for _, strategy := range []string{"tradepulse", "autowheel"} {
		path := filepath.Join(s.Config.SocketDir, strategy+".sock")
		if info, err := os.Lstat(path); err == nil {
			if info.Mode()&os.ModeSocket == 0 {
				return errors.New("refusing to replace a non-socket coordinator path")
			}
			if err = os.Remove(path); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		listener, err := net.Listen("unix", path)
		if err != nil {
			return err
		}
		listeners = append(listeners, listener)
		if err = os.Chmod(path, 0600); err != nil {
			return err
		}
		wg.Go(func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				wg.Go(func() { s.serveConnection(ctx, strategy, conn) })
			}
		})
	}
	wg.Go(func() {
		<-ctx.Done()
		for _, listener := range listeners {
			listener.Close()
		}
	})
	poll := time.NewTicker(60 * time.Second)
	defer poll.Stop()
	// Read-only observation runs before accepting the first view. The ledger lock
	// remains held throughout the service lifetime even in observation mode.
	s.refresh(ctx)
	for {
		// Protective management has priority over waiting admissions. It cannot
		// interrupt a financial RPC whose durable send already began.
		select {
		case j := <-s.management:
			s.execute(j)
			continue
		default:
		}
		select {
		case <-ctx.Done():
			wg.Wait()
			return nil
		case j := <-s.management:
			s.execute(j)
		case j := <-s.entries:
			s.execute(j)
		case <-poll.C:
			s.refresh(ctx)
		}
	}
}
func (s *Server) refresh(ctx context.Context) (sharedbroker.Facts, error) {
	facts, err := s.Broker.Snapshot(ctx)
	if err == nil {
		err = s.reconcileTerminalStocks(facts)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.lastError = err.Error()
		return facts, err
	}
	s.latest = facts
	s.lastError = ""
	// Preserve broker evidence privately; failures revoke health rather than
	// permitting execution from an in-memory observation only.
	if err = aruntime.AtomicJSON(filepath.Join(s.Config.SocketDir, "observed-account.json"), facts); err != nil {
		s.lastError = "broker evidence could not be persisted"
		return facts, err
	}
	return facts, nil
}
func reply(ok bool, data any, reason string, unsent bool) sharedclient.Response {
	raw, err := json.Marshal(data)
	if err != nil {
		return sharedclient.Response{Error: "coordinator response could not be encoded", Unsent: unsent}
	}
	return sharedclient.Response{OK: ok, Data: raw, Error: reason, Unsent: unsent}
}
func (s *Server) serveConnection(parent context.Context, strategy string, conn net.Conn) {
	defer conn.Close()
	ctx, cancel := context.WithTimeout(parent, 35*time.Second)
	defer cancel()
	conn.SetDeadline(time.Now().Add(35 * time.Second))
	reader := bufio.NewReader(io.LimitReader(conn, 1<<20+1))
	line, err := reader.ReadBytes('\n')
	if err != nil || len(line) > 1<<20 {
		json.NewEncoder(conn).Encode(reply(false, nil, "coordinator request missing or oversized", true))
		return
	}
	var r sharedclient.Request
	if err = decode(line, &r); err != nil || r.Version != 1 || r.PolicyHash != s.Config.PolicyHash || r.Release != s.Config.Releases[strategy] {
		json.NewEncoder(conn).Encode(reply(false, nil, "strategy release or policy binding mismatch", true))
		return
	}
	if err = authenticatePeer(conn, r.Release); err != nil {
		slog.Error("coordinator peer identity rejected", "strategy", strategy, "reason", err.Error())
		json.NewEncoder(conn).Encode(reply(false, nil, "strategy process identity cannot be verified", true))
		return
	}
	// One request per connection. EOF means the requesting strategy has gone
	// away; cancel queued work and broker reads before any durable send begins.
	// Both clients retain their write side until the response is complete.
	go func() { reader.ReadByte(); cancel() }()
	j := job{strategy: strategy, request: r, reply: make(chan sharedclient.Response, 1), ctx: ctx}
	queue := s.entries
	if r.Op == "permission" || r.Op == "withdraw" || r.Op == "cancel_stock" {
		queue = s.management
	}
	if r.Op == "prepare_stock" {
		var p struct {
			Request struct {
				Side string `json:"side"`
			} `json:"request"`
		}
		if json.Unmarshal(r.Payload, &p) == nil && p.Request.Side == "sell" {
			queue = s.management
		}
	}
	if r.Op == "commit_stock" {
		var p struct {
			Key string `json:"intent_key"`
		}
		if json.Unmarshal(r.Payload, &p) == nil {
			if in, err := s.Ledger.Lookup(strategy, p.Key); err == nil && in.Request.Kind == "stock_sell" {
				queue = s.management
			}
		}
	}
	select {
	case queue <- j:
	case <-ctx.Done():
		return
	}
	select {
	case result := <-j.reply:
		json.NewEncoder(conn).Encode(result)
	case <-ctx.Done():
		return
	}
}
func decode(raw []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	d.UseNumber()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}
func (s *Server) execute(j job) {
	if j.ctx.Err() != nil {
		j.reply <- s.denyBeforeSend(j.strategy, j.request, "coordinator request expired before admission")
		return
	}
	result := s.handle(j.ctx, j.strategy, j.request)
	j.reply <- result
}
func (s *Server) health(strategy string, facts sharedbroker.Facts, st coordinator.Status) (status sharedclient.Status) {
	defer func() {
		for _, blocker := range s.Config.PendingQualification {
			if !slices.Contains(status.Blockers, blocker) {
				status.Blockers = append(status.Blockers, blocker)
			}
		}
		if len(s.Config.PendingQualification) > 0 {
			status.Ready = false
			if status.Reason == "" {
				status.Reason = s.Config.PendingQualification[0]
			}
		}
	}()
	status = sharedclient.Status{AccountHash: facts.AccountHash, PolicyHash: s.Config.PolicyHash, Release: s.Config.Releases[strategy], At: facts.At, Generation: st.Generation, AvailableCash: min(facts.EligibleCash, st.FreeCashCents), Capital: st.CapitalCents}
	if facts.Account != nil {
		account := sharedbroker.Text(facts.Account, "account_number")
		status.BrokerSummary = &robinhood.AccountInfo{At: facts.At, AccountLast4: account[max(0, len(account)-4):], OptionsLevel: sharedbroker.Text(facts.Account, "option_level"), AvailableCash: wheel.Cents(facts.EligibleCash), SnapshotComplete: true}
		for _, row := range facts.Positions {
			status.BrokerSummary.Holdings = append(status.BrokerSummary.Holdings, robinhood.HoldingInfo{Symbol: sharedbroker.Text(row, "symbol"), Quantity: sharedbroker.Text(row, "quantity"), Available: sharedbroker.Text(row, "shares_available_for_sells")})
		}
	}
	for _, r := range st.Registrations {
		if r.Strategy == strategy {
			status.Epoch = r.Epoch
			status.NewRisk = r.NewRisk
			status.Management = r.Management
		}
	}
	if !st.Initialized {
		status.Reason = "frozen TradePulse ownership import has not been verified"
		status.Blockers = []string{status.Reason}
		return status
	}
	if st.AccountHash != facts.AccountHash || st.PolicyHash != s.Config.PolicyHash {
		status.Reason = "imported account or policy differs from broker facts"
		status.Blockers = []string{status.Reason}
		return status
	}
	for _, registration := range st.Registrations {
		if registration.Release != s.Config.Releases[registration.Strategy] || registration.PolicyHash != s.Config.PolicyHash {
			status.Reason = "strategy release differs from the frozen import"
			status.Blockers = []string{status.Reason}
			return status
		}
	}
	if err := reconcile(st, facts); err != nil {
		status.Reason = err.Error()
		status.Blockers = []string{status.Reason}
		return status
	}
	status.Reconciled = true
	for _, owner := range st.Owners {
		if owner.Strategy != strategy {
			status.PeerSymbols = append(status.PeerSymbols, owner.Symbol)
		}
		status.HeldCash += owner.CashHoldCents
	}
	for _, in := range st.Intents {
		if in.State != "terminal" && in.State != "withdrawn" && in.State != "cancel_terminal" {
			status.HeldCash += in.Request.CashCents
		}
	}
	slices.Sort(status.PeerSymbols)
	if s.Config.Qualification == nil {
		status.Reason = "shared cutover and execution policy have not been qualified"
		status.Blockers = []string{status.Reason}
		return status
	}
	if err := s.Config.Qualification(st, facts); err != nil {
		status.Reason = err.Error()
		status.Blockers = []string{status.Reason}
		return status
	}
	status.SingleWriter = true
	if st.AccountPaused || st.BlockedReason != "" {
		status.Reason = "account execution is paused or unresolved"
		status.Blockers = []string{status.Reason}
		return status
	}
	status.Ready = true
	return status
}
func (s *Server) handle(ctx context.Context, strategy string, r sharedclient.Request) sharedclient.Response {
	if r.Op == "commit_stock" || r.Op == "commit_option" {
		var p struct {
			Key    string `json:"intent_key"`
			Digest string `json:"request_digest"`
			Ticket int64  `json:"ticket_version"`
		}
		if decode(r.Payload, &p) == nil {
			if prior, err := s.Ledger.Lookup(strategy, p.Key); err == nil && prior.Request.Digest == p.Digest && (prior.State == "accepted" || prior.State == "terminal") && len(prior.Response) > 0 {
				return reply(true, json.RawMessage(prior.Response), "", false)
			}
		}
	}
	// Permission reductions are durable even when the broker is unavailable.
	// Otherwise a successful local pause could leave an old commit admissible
	// when broker health recovers. Grants still require the normal health gate.
	if r.Op == "permission" {
		var p struct {
			NewRisk    bool `json:"new_risk"`
			Management bool `json:"management"`
		}
		if decode(r.Payload, &p) != nil || strategy == "autowheel" && p.Management {
			return reply(false, nil, "invalid strategy permission", true)
		}
		st, err := s.Ledger.Status()
		if err != nil {
			return reply(false, nil, "account ledger unavailable", true)
		}
		for _, registration := range st.Registrations {
			if registration.Strategy == strategy && (!p.NewRisk || registration.NewRisk) && (!p.Management || registration.Management) {
				return s.setPermission(strategy, p.NewRisk, p.Management)
			}
		}
	}
	if r.Op == "status" {
		s.mu.RLock()
		facts, problem := s.latest, s.lastError
		s.mu.RUnlock()
		st, err := s.Ledger.Status()
		if err != nil {
			return reply(false, nil, "account ledger unavailable", true)
		}
		status := s.health(strategy, facts, st)
		if problem != "" {
			status.Ready = false
			status.Reconciled = false
			status.Reason = problem
			status.Blockers = []string{problem}
		}
		if time.Since(facts.At) > 90*time.Second || time.Until(facts.At) > 2*time.Second {
			status.Ready = false
			status.Reconciled = false
			status.Reason = "complete broker observation is stale"
		}
		return reply(true, status, "", true)
	}
	if r.Op == "intent" || r.Op == "withdraw" {
		var p struct {
			Key string `json:"intent_key"`
		}
		if decode(r.Payload, &p) != nil {
			return reply(false, nil, "invalid intent lookup", true)
		}
		if r.Op == "withdraw" {
			if err := s.Ledger.Withdraw(strategy, p.Key); err != nil {
				return reply(false, nil, err.Error(), false)
			}
		}
		in, err := s.Ledger.Lookup(strategy, p.Key)
		if err != nil {
			return reply(false, nil, "durable intent unavailable", true)
		}
		return reply(true, in, "", in.State == "withdrawn" || in.State == "prepared" || in.State == "reserved")
	}
	facts, err := s.refresh(ctx)
	if err != nil {
		return s.denyBeforeSend(strategy, r, "complete broker observation unavailable: "+err.Error())
	}
	st, err := s.Ledger.Status()
	if err != nil {
		return reply(false, nil, "durable account ledger unavailable", true)
	}
	status := s.health(strategy, facts, st)
	if r.Op == "view" {
		if !status.Ready {
			return reply(false, nil, status.Reason, true)
		}
		view, err := ownedView(strategy, st, facts, status)
		if err != nil {
			return reply(false, nil, err.Error(), true)
		}
		return reply(true, view, "", true)
	}
	if !status.Ready {
		return s.denyBeforeSend(strategy, r, status.Reason)
	}
	switch r.Op {
	case "permission":
		var p struct {
			NewRisk    bool `json:"new_risk"`
			Management bool `json:"management"`
		}
		if decode(r.Payload, &p) != nil || strategy == "autowheel" && p.Management {
			return reply(false, nil, "invalid strategy permission", true)
		}
		return s.setPermission(strategy, p.NewRisk, p.Management)
	case "cancel_stock":
		return s.cancelStock(ctx, strategy, r.Payload, facts, st)
	case "prepare_stock", "prepare_option":
		if strategy == "autowheel" && r.Op != "prepare_option" || strategy == "tradepulse" && r.Op != "prepare_stock" {
			return reply(false, nil, "strategy financial capability denied", true)
		}
		request, err := s.Broker.Plan(ctx, strategy, r.Payload)
		if err != nil {
			return reply(false, nil, err.Error(), true)
		}
		in, err := s.Ledger.Reserve(strategy, request, facts.EligibleCash, time.Now())
		if err != nil {
			return reply(false, nil, err.Error(), true)
		}
		if in.State != "reserved" && in.State != "prepared" {
			return reply(false, nil, "intent already started or conclusively withdrawn", in.State == "withdrawn")
		}
		review, expiry, err := s.Broker.Review(ctx, in)
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			if cleanup := s.Ledger.Withdraw(strategy, request.IntentKey); cleanup != nil {
				return reply(false, nil, "unsent review reservation could not be withdrawn", false)
			}
			return reply(false, nil, err.Error(), true)
		}
		in, err = s.Ledger.ReviewPrepared(strategy, request.IntentKey, sharedclient.Digest(review), "", review, expiry)
		if err != nil {
			if cleanup := s.Ledger.Withdraw(strategy, request.IntentKey); cleanup != nil {
				return reply(false, nil, "failed preparation could not be withdrawn; intent retained", false)
			}
			return reply(false, nil, err.Error(), true)
		}
		return reply(true, map[string]any{"intent_key": request.IntentKey, "request_digest": request.Digest, "ticket_version": in.TicketVersion, "review": json.RawMessage(review)}, "", true)
	case "commit_stock", "commit_option":
		var p struct {
			Key    string `json:"intent_key"`
			Digest string `json:"request_digest"`
			Ticket int64  `json:"ticket_version"`
		}
		if decode(r.Payload, &p) != nil {
			return reply(false, nil, "invalid exact commit ticket", true)
		}
		prior, err := s.Ledger.Lookup(strategy, p.Key)
		if err != nil {
			return reply(false, nil, "durable preparation missing", true)
		}
		if strategy == "autowheel" && prior.Request.Kind != "put" && prior.Request.Kind != "call" || strategy == "tradepulse" && prior.Request.Kind != "stock_buy" && prior.Request.Kind != "stock_sell" {
			return reply(false, nil, "commit capability denied", true)
		}
		// The receipt can be replayed to a disconnected client; the transport cannot.
		if prior.State == "accepted" && prior.Request.Digest == p.Digest {
			return reply(true, json.RawMessage(prior.Response), "", false)
		}
		if prior.Request.CashCents > facts.EligibleCash {
			return s.denyBeforeSend(strategy, r, "fresh broker cash cannot cover the prepared debit")
		}
		if ctx.Err() != nil {
			return s.denyBeforeSend(strategy, r, "requesting strategy disconnected or its deadline expired")
		}
		in, err := s.Ledger.StartSend(strategy, p.Key, p.Ticket, p.Digest, time.Now())
		if err != nil {
			return s.denyBeforeSend(strategy, r, err.Error())
		}
		id, raw, err := s.Broker.PlaceOnce(ctx, in)
		if err != nil || id == "" {
			s.Ledger.MarkUncertain(strategy, p.Key)
			return reply(false, nil, "financial transport outcome uncertain; original intent retained", false)
		}
		if err = s.Ledger.Complete(strategy, p.Key, id, raw); err != nil {
			s.Ledger.MarkUncertain(strategy, p.Key)
			return reply(false, nil, "receipt could not be bound durably; resources retained", false)
		}
		return reply(true, json.RawMessage(raw), "", false)
	default:
		return reply(false, nil, "coordinator operation unsupported", true)
	}
}

func (s *Server) denyBeforeSend(strategy string, r sharedclient.Request, reason string) sharedclient.Response {
	if r.Op != "commit_stock" && r.Op != "commit_option" {
		return reply(false, nil, reason, true)
	}
	var p struct {
		Key    string `json:"intent_key"`
		Digest string `json:"request_digest"`
		Ticket int64  `json:"ticket_version"`
	}
	if decode(r.Payload, &p) != nil {
		return reply(false, nil, reason, false)
	}
	prior, err := s.Ledger.Lookup(strategy, p.Key)
	if err != nil {
		return reply(false, nil, reason, false)
	}
	if prior.Request.Digest != p.Digest {
		return reply(false, nil, reason, false)
	}
	if prior.State == "withdrawn" {
		return reply(false, nil, reason, true)
	}
	if prior.State != "prepared" && prior.State != "reserved" {
		return reply(false, nil, reason, false)
	}
	if s.Ledger.Withdraw(strategy, p.Key) != nil {
		return reply(false, nil, "unsent commit could not be conclusively withdrawn; intent retained", false)
	}
	return reply(false, nil, reason, true)
}

func (s *Server) setPermission(strategy string, newRisk, management bool) sharedclient.Response {
	if err := s.Ledger.SetPermission(strategy, newRisk, management); err != nil {
		return reply(false, nil, "strategy permission could not be persisted", true)
	}
	updated, err := s.Ledger.Status()
	if err != nil {
		return reply(false, nil, "persisted permission cannot be read", true)
	}
	for _, registration := range updated.Registrations {
		if registration.Strategy == strategy {
			return reply(true, map[string]any{"applied": true, "permission_epoch": registration.Epoch}, "", true)
		}
	}
	return reply(false, nil, "persisted permission binding missing", true)
}

func (s *Server) cancelStock(ctx context.Context, strategy string, payload json.RawMessage, facts sharedbroker.Facts, st coordinator.Status) sharedclient.Response {
	var p struct {
		ID string `json:"order_id"`
	}
	if strategy != "tradepulse" || decode(payload, &p) != nil || p.ID == "" {
		return reply(false, nil, "invalid owned stock cancellation", true)
	}
	key := "cancel:" + p.ID
	if prior, err := s.Ledger.Lookup(strategy, key); err == nil {
		if prior.State == "cancel_pending" || prior.State == "cancel_terminal" {
			return reply(true, json.RawMessage(prior.Response), "", false)
		}
		return reply(false, nil, "original cancellation outcome retained; transport cannot be retried", false)
	}
	var original coordinator.Intent
	for _, in := range st.Intents {
		if in.Strategy == strategy && in.BrokerOrderID == p.ID && in.State == "accepted" {
			original = in
		}
	}
	if original.Request.IntentKey == "" {
		return reply(false, nil, "cancellation requires an active exact owned receipt", true)
	}
	request := coordinator.Request{IntentKey: key, RefID: key, UnderlyingID: original.Request.UnderlyingID, Symbol: original.Request.Symbol, Kind: "cancel", OriginalOrderID: p.ID, Digest: sharedclient.Digest(payload), Payload: payload}
	if _, err := s.Ledger.Reserve(strategy, request, facts.EligibleCash, time.Now()); err != nil {
		return reply(false, nil, err.Error(), true)
	}
	review, err := json.Marshal(map[string]any{"original_order_id": p.ID, "complete_observation_digest": facts.Digest})
	if err != nil {
		return reply(false, nil, "cancellation evidence cannot be encoded", true)
	}
	ticket, err := s.Ledger.ReviewPrepared(strategy, key, sharedclient.Digest(review), "", review, facts.At.Add(30*time.Second))
	if err != nil {
		if s.Ledger.Withdraw(strategy, key) != nil {
			return reply(false, nil, "unsent cancellation could not be withdrawn", false)
		}
		return reply(false, nil, err.Error(), true)
	}
	in, err := s.Ledger.StartSend(strategy, key, ticket.TicketVersion, request.Digest, time.Now())
	if err != nil {
		if s.Ledger.Withdraw(strategy, key) != nil {
			return reply(false, nil, "unsent cancellation could not be withdrawn", false)
		}
		return reply(false, nil, err.Error(), true)
	}
	raw, err := s.Broker.CancelOnce(ctx, in)
	if err != nil {
		s.Ledger.MarkUncertain(strategy, key)
		return reply(false, nil, "cancel outcome uncertain; original obligations retained", false)
	}
	if err = s.Ledger.CompleteCancel(strategy, key, raw); err != nil {
		s.Ledger.MarkUncertain(strategy, key)
		return reply(false, nil, "cancel receipt not durably recorded; obligations retained", false)
	}
	return reply(true, json.RawMessage(raw), "", false)
}

func reconcile(st coordinator.Status, f sharedbroker.Facts) error {
	if time.Since(f.At) > 30*time.Second || time.Until(f.At) > 2*time.Second {
		return errors.New("full account observation is stale")
	}
	owned := make(map[string]coordinator.Ownership)
	for _, o := range st.Owners {
		owned[o.Symbol] = o
	}
	observed := make(map[string]int64)
	for _, p := range f.Positions {
		symbol := sharedbroker.Text(p, "symbol")
		quantity, err := sharedbroker.Micros(sharedbroker.Text(p, "quantity"))
		if err != nil {
			return err
		}
		if quantity == 0 {
			continue
		}
		owner, ok := owned[symbol]
		if !ok || owner.QuantityMicros != quantity {
			return errors.New("broker stock inventory does not match durable ownership")
		}
		observed[symbol] = quantity
	}
	for _, o := range st.Owners {
		if observed[o.Symbol] != o.QuantityMicros {
			return errors.New("owned stock inventory disappeared or changed")
		}
	}
	known := make(map[string]coordinator.Intent)
	baseline := make(map[string]coordinator.HistoricalOrder)
	for _, order := range st.History {
		baseline[order.Kind+":"+order.ID] = order
	}
	for _, in := range st.Intents {
		if in.State == "send_started" || in.State == "uncertain" || in.State == "cancel_started" {
			return errors.New("account has unresolved financial transport")
		}
		if in.BrokerOrderID != "" {
			known[in.BrokerOrderID] = in
		}
	}
	for _, order := range f.Orders {
		in, ok := known[sharedbroker.Text(order, "id")]
		if ok && in.Request.Symbol == sharedbroker.Text(order, "symbol") {
			continue
		}
		raw, _ := json.Marshal(order)
		prior, ok := baseline["equity:"+sharedbroker.Text(order, "id")]
		if !ok || !sharedbroker.Terminal(sharedbroker.Text(order, "state")) || sharedclient.Digest(raw) != prior.Digest {
			return errors.New("stock activity is not explained by an exact owned intent or frozen history")
		}
	}
	for _, order := range f.OptionOrders {
		in, ok := known[sharedbroker.Text(order, "id")]
		if ok && in.Request.Symbol == sharedbroker.Text(order, "chain_symbol") {
			continue
		}
		raw, _ := json.Marshal(order)
		prior, ok := baseline["option:"+sharedbroker.Text(order, "id")]
		if !ok || !sharedbroker.Terminal(sharedbroker.Text(order, "state")) || sharedclient.Digest(raw) != prior.Digest {
			return errors.New("option activity is not explained by an exact owned intent or frozen history")
		}
	}
	for _, order := range f.CryptoOrders {
		raw, _ := json.Marshal(order)
		prior, ok := baseline["crypto:"+sharedbroker.Text(order, "id")]
		if !ok || !sharedbroker.Terminal(sharedbroker.Text(order, "state")) || sharedclient.Digest(raw) != prior.Digest {
			return errors.New("crypto activity differs from frozen history")
		}
	}
	for _, position := range f.Options {
		q, err := sharedbroker.Micros(sharedbroker.Text(position, "quantity"))
		if err != nil {
			return err
		}
		if q != 0 {
			return errors.New("option lifecycle reconciliation has not yet qualified this broker exposure")
		}
		for _, field := range []string{"pending_assignment_quantity", "pending_expiration_quantity", "pending_exercise_quantity", "pending_buy_quantity", "pending_sell_quantity"} {
			value, err := sharedbroker.Micros(sharedbroker.Text(position, field))
			if err != nil || value != 0 {
				return errors.New("option settlement facts are missing or pending")
			}
		}
	}
	return nil
}
func ownedView(strategy string, st coordinator.Status, f sharedbroker.Facts, status sharedclient.Status) (sharedclient.View, error) {
	owners := make(map[string]bool)
	for _, o := range st.Owners {
		if o.Strategy == strategy {
			owners[o.Symbol] = true
		}
	}
	positions := make([]sharedbroker.Object, 0)
	orders := make([]sharedbroker.Object, 0)
	options := make([]sharedbroker.Object, 0)
	optionOrders := make([]sharedbroker.Object, 0)
	var peerCapital int64
	peers := make([]string, 0)
	for _, o := range st.Owners {
		if o.Strategy != strategy {
			peerCapital += o.CostCents + o.PutCollateralCents
			peers = append(peers, o.Symbol)
		}
	}
	for _, in := range st.Intents {
		if in.Strategy != strategy && in.State != "terminal" && in.State != "withdrawn" && in.State != "cancel_terminal" {
			peerCapital += in.Request.CapitalCents
		}
	}
	slices.Sort(peers)
	for _, p := range f.Positions {
		if owners[sharedbroker.Text(p, "symbol")] {
			positions = append(positions, p)
		}
	}
	ids := make(map[string]bool)
	for _, order := range st.History {
		if order.Strategy == strategy {
			ids[order.ID] = true
		}
	}
	for _, in := range st.Intents {
		if in.Strategy == strategy && in.BrokerOrderID != "" {
			ids[in.BrokerOrderID] = true
		}
	}
	for _, o := range f.Orders {
		if ids[sharedbroker.Text(o, "id")] {
			orders = append(orders, o)
		}
	}
	for _, o := range f.OptionOrders {
		if ids[sharedbroker.Text(o, "id")] {
			optionOrders = append(optionOrders, o)
		}
	}
	for _, o := range f.Options {
		if owners[sharedbroker.Text(o, "chain_symbol")] {
			options = append(options, o)
		}
	}
	full, err := json.Marshal(f)
	if err != nil {
		return sharedclient.View{}, err
	}
	owned, err := json.Marshal(map[string]any{"account": f.Account, "accounts": f.Accounts, "portfolio": f.Portfolio, "positions": map[string]any{"positions": positions}, "orders": map[string]any{"orders": orders}, "option_positions": map[string]any{"positions": options}, "option_orders": map[string]any{"orders": optionOrders}, "available_cash_cents": status.AvailableCash, "peer_capital_cents": peerCapital, "peer_symbols": peers})
	if err != nil {
		return sharedclient.View{}, err
	}
	epoch := int64(0)
	for _, r := range st.Registrations {
		if r.Strategy == strategy {
			epoch = r.Epoch
		}
	}
	return sharedclient.View{Evidence: sharedclient.Evidence{Schema: "SharedAccountEvidenceV1", AccountHash: f.AccountHash, PolicyHash: status.PolicyHash, Release: status.Release, FullHash: sharedclient.Digest(full), OwnedHash: sharedclient.Digest(owned), At: f.At, Generation: st.Generation, Epoch: epoch, Reconciled: status.Reconciled, Qualified: status.Ready, SingleWriter: status.SingleWriter}, FullJSON: string(full), OwnedJSON: string(owned)}, nil
}
