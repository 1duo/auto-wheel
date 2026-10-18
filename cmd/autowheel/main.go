package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata"

	"autowheel/internal/robinhood"
	aruntime "autowheel/internal/runtime"
	"autowheel/internal/sharedclient"
	"autowheel/internal/wheel"
)

var version = "dev"

type status struct {
	TradingWindowOpen     bool                      `json:"trading_window_open"`
	Version               string                    `json:"version"`
	Mode                  string                    `json:"mode"`
	Stage                 string                    `json:"stage"`
	Reason                string                    `json:"reason,omitempty"`
	At                    time.Time                 `json:"at"`
	Candidates            []wheel.Candidate         `json:"candidates,omitempty"`
	Positions             map[string]wheel.Position `json:"positions,omitempty"`
	Intents               map[string]wheel.Intent   `json:"intents,omitempty"`
	Broker                *wheel.Snapshot           `json:"broker,omitempty"`
	AccountInfo           *robinhood.AccountInfo    `json:"account_info,omitempty"`
	Config                wheel.Config              `json:"config"`
	AccountConfigured     bool                      `json:"account_configured"`
	TokenPresent          bool                      `json:"token_present"`
	Halted                bool                      `json:"halted"`
	ServerTime            time.Time                 `json:"server_time"`
	LiveCapable           bool                      `json:"live_capable"`
	LiveReadiness         *liveReadiness            `json:"live_readiness,omitempty"`
	Coordinator           *sharedclient.Status      `json:"coordinator,omitempty"`
	PendingSelectionMode  wheel.SelectionMode       `json:"pending_selection_mode,omitempty"`
	PendingStrategyPreset string                    `json:"pending_strategy_preset,omitempty"`
	FixedBatch            *wheel.FixedBatch         `json:"fixed_signal_batch,omitzero"`
	TakeProfitBatch       *wheel.FixedCloseBatch    `json:"take_profit_batch,omitzero"`
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := run(); err != nil {
		slog.Error("AutoWheel stopped", "reason", err.Error())
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return errors.New("usage: autowheel version|init|login|check-config|check-transition|rebind-capital|enable-tp75|doctor|audit-account|run|once|adopt|recover")
	}
	cmd := os.Args[1]
	if cmd == "version" {
		fmt.Println("AutoWheel", version)
		return nil
	}
	f := flag.NewFlagSet(cmd, flag.ContinueOnError)
	config := f.String("config", "config.json", "configuration file")
	stateDir := f.String("state", "state", "private state directory")
	token := f.String("token", "", "private OAuth token file; defaults to state/robinhood.token.json")
	listen := f.String("listen", "127.0.0.1:9841", "loopback status address")
	live := f.Bool("live", false, "enable order transport (operator activation only)")
	exclusive := f.Bool("exclusive-account", false, "legacy exclusive-account assertion")
	liveCapable := f.Bool("live-capable", false, "allow readiness-gated native activation; does not enable trading")
	port := f.Int("callback-port", 8765, "OAuth loopback callback port")
	symbol := f.String("symbol", "", "assignment symbol")
	cost := f.String("cost", "", "assignment cost per share, dollars")
	orderID := f.String("order-id", "", "broker-confirmed order ID for lost-receipt recovery")
	receipt := f.String("receipt", "", "broker assignment receipt reference")
	if err := f.Parse(os.Args[2:]); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *token == "" {
		*token = filepath.Join(*stateDir, "robinhood.token.json")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if cmd == "init" {
		if _, err := os.Lstat(*config); !errors.Is(err, os.ErrNotExist) {
			return errors.New("init requires a new configuration path")
		}
		initial, err := wheel.DefaultConfig().WithStrategyPreset("conservative-roi-v1")
		if err != nil {
			return err
		}
		return aruntime.AtomicJSON(*config, initial)
	}
	if cmd == "login" {
		if *port < 1024 || *port > 65535 {
			return errors.New("callback port must be 1024–65535")
		}
		j, _, err := aruntime.OpenJournal(*stateDir)
		if err != nil {
			return err
		}
		defer j.Close()
		ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
		defer cancel()
		return robinhood.Login(ctx, *token, *port, func(u string) { fmt.Println("Open this authorization URL in your browser:\n" + u) })
	}
	c, err := loadConfig(*config)
	if err != nil {
		return err
	}
	c, err = loadSelectionOverride(c, *stateDir)
	if err != nil {
		return err
	}
	if cmd == "check-config" {
		if *live || *liveCapable || *exclusive {
			return errors.New("configuration check cannot enable trading")
		}
		return writeConfigCheck(os.Stdout, c)
	}
	if cmd == "check-transition" {
		if *live || *liveCapable || *exclusive {
			return errors.New("transition inspection cannot enable trading")
		}
		j, st, err := aruntime.OpenJournal(*stateDir)
		if err != nil {
			return err
		}
		defer j.Close()
		if st.Account != c.Account {
			return errors.New("transition journal and configured account differ")
		}
		digest, err := wheel.FixedLegacyDigest(st)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"scope":               "journal_only; fresh broker agreement required before import",
			"legacy_state_sha256": digest, "owned_symbols": slices.Sorted(maps.Keys(st.Positions)),
			"broker_verified": false, "trading_authorized": false})
	}
	if cmd == "rebind-capital" {
		if *live || *liveCapable || *exclusive {
			return errors.New("capital transition cannot enable trading")
		}
		return rebindCapital(c, *stateDir, os.Stdout)
	}
	if cmd == "enable-tp75" {
		if *live || *liveCapable || *exclusive {
			return errors.New("TP75 transition cannot enable trading")
		}
		return enableTP75(c, *stateDir, os.Stdout)
	}
	if cmd == "audit-account" {
		if *live || *liveCapable || *exclusive {
			return errors.New("account audit is read-only")
		}
		return auditAccount(ctx, c, *token, *stateDir)
	}
	if *liveCapable && c.TradePulseStateDir == "" {
		return errors.New("live-capable deployment requires tradepulse_state_dir for account isolation")
	}
	if cmd == "doctor" {
		if *live {
			return errors.New("doctor is read-only")
		}
		j, _, err := aruntime.OpenJournal(*stateDir)
		if err != nil {
			return err
		}
		defer j.Close()
		client := robinhood.NewClient(*token, false)
		if err = client.Connect(ctx); err != nil {
			return err
		}
		inventory := client.Inventory()
		slices.SortFunc(inventory, func(a, b robinhood.Tool) int {
			if a.Name < b.Name {
				return -1
			}
			if a.Name > b.Name {
				return 1
			}
			return 0
		})
		if err = aruntime.AtomicJSON(filepath.Join(*stateDir, "mcp-tools.json"), inventory); err != nil {
			return err
		}
		accounts, err := client.Call(ctx, "get_accounts", map[string]any{})
		if err != nil {
			return err
		}
		if err = aruntime.AtomicJSON(filepath.Join(*stateDir, "mcp-accounts.json"), accounts); err != nil {
			return err
		}
		if c.Account == "" {
			fmt.Println("MCP authenticated; schemas and account discovery saved privately. Configure an explicit agentic account number.")
			return nil
		}
		broker := &robinhood.Broker{MCP: client, Config: c}
		_, err = broker.Snapshot(ctx)
		if err != nil {
			return err
		}
		fmt.Println("Read-only account snapshot validated; order and assignment acceptance remains required.")
		return nil
	}
	if cmd != "run" && cmd != "once" && cmd != "adopt" && cmd != "recover" {
		return errors.New("unknown command")
	}
	var retired *retiredPeer
	if c.RetiredTradePulse != "" {
		retired, err = openRetiredPeer(c, retiredTradePulseService)
		if err != nil {
			return err
		}
		defer retired.close()
	}
	if *live && (c.Account == "" || !*exclusive) {
		return errors.New("live requires configured account and explicit exclusive-account assertion")
	}
	j, st, err := aruntime.OpenJournal(*stateDir)
	if err != nil {
		return err
	}
	defer j.Close()
	if st.Account != "" && st.Account != c.Account {
		return errors.New("configuration account differs from durable journal")
	}
	st.Account = c.Account
	if err = j.Save(st); err != nil {
		return err
	}
	var fixed *wheel.FixedController
	if c.FixedPolicy != nil {
		fixed, err = wheel.NewFixedController(c.FixedPolicy.Spec, &st, j)
		if err != nil {
			return err
		}
	}
	if cmd == "adopt" || cmd == "recover" {
		if cmd == "adopt" && (c.FixedPolicy != nil || st.Fixed != nil) {
			return errors.New("fixed policy cannot adopt external or legacy stock")
		}
		if cmd == "adopt" && c.CoordinatorSocket != "" {
			return errors.New("shared account inventory must be adopted through the coordinator's ownership proof")
		}
		if *live {
			return errors.New("adopt does not submit orders")
		}
		client := robinhood.NewClient(*token, false)
		if err = client.Connect(ctx); err != nil {
			return err
		}
		b := &robinhood.Broker{MCP: client, Config: c}
		snapshot, err := b.Snapshot(ctx)
		if err != nil {
			return err
		}
		if cmd == "recover" {
			in, ok := st.Intents[*symbol]
			if !ok || *orderID == "" || (in.OrderID != "" && in.OrderID != *orderID) {
				return errors.New("recovery requires an unresolved symbol and its confirmed broker ID")
			}
			if err = b.RecoverReceipt(ctx, in.Candidate, *orderID); err != nil {
				return err
			}
			in.OrderID = *orderID
			st.Intents[*symbol] = in
			return j.Save(st)
		}
		if err = tradePulseIsolation(c, retired); err != nil {
			return err
		}
		n, err := wheel.ParseCents(*cost)
		if err != nil {
			return err
		}
		if err = wheel.Adopt(c, &st, snapshot, *symbol, *receipt, n); err != nil {
			return err
		}
		return j.Save(st)
	}
	mode := "read_only"
	if *live {
		mode = "live"
	}
	var mu sync.RWMutex
	var lastBroker *wheel.Snapshot
	var lastAccountInfo *robinhood.AccountInfo
	var readiness *liveReadiness
	activation := &activationControl{Capable: *liveCapable, Config: c, Dir: *stateDir, RetiredPeer: retired}
	var submitMu sync.Mutex
	var cycleMu sync.Mutex
	if *live {
		return errors.New("use --live-capable and the readiness-gated native switch")
	}
	wake := make(chan struct{}, 1)
	current := status{Version: version, Mode: mode, Stage: "starting", At: time.Now()}
	update := func(s status) {
		s.Positions = maps.Clone(st.Positions)
		s.Intents = maps.Clone(st.Intents)
		s.Broker = lastBroker
		s.AccountInfo = lastAccountInfo
		s.LiveCapable = activation.Capable
		s.LiveReadiness = readiness
		if st.Fixed != nil {
			s.FixedBatch = st.Fixed.Batch
			s.TakeProfitBatch = st.Fixed.TakeProfit
		}
		mu.Lock()
		current = s
		mu.Unlock()
		slog.Info("cycle", "mode", s.Mode, "stage", s.Stage, "reason", s.Reason, "candidates", len(s.Candidates))
	}
	cycle := func() error {
		cycleMu.Lock()
		defer cycleMu.Unlock()
		if activation.profitOnly() != "" {
			submitMu.Lock()
			err := pauseTrading(activation, *stateDir, "close-only session ended; fresh operator action required")
			submitMu.Unlock()
			if err != nil {
				return err
			}
		}
		s := status{Version: version, Mode: mode, Stage: "idle", At: time.Now(), Positions: st.Positions}
		if activation.Capable {
			if c.CoordinatorSocket != "" {
				coordinated, err := sharedStatus(c)
				if err == nil {
					s.Coordinator = &coordinated
					if coordinated.BrokerSummary != nil {
						lastAccountInfo = coordinated.BrokerSummary
					}
				}
			}
			submitMu.Lock()
			isolationErr := activation.fenceIsolation()
			submitMu.Unlock()
			if isolationErr != nil {
				readiness = &liveReadiness{At: time.Now(), Reason: isolationErr.Error()}
			}
		}
		if activation.enabled() {
			s.Mode = "live"
		} else {
			s.Mode = "read_only"
		}
		_, haltErr := os.Lstat(filepath.Join(*stateDir, "HALT"))
		halted := !errors.Is(haltErr, os.ErrNotExist)
		if c.Account == "" {
			s.Stage = "waiting_configuration"
			s.Reason = "explicit Robinhood agentic account number required"
			update(s)
			return nil
		}
		cycleCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		client := robinhood.NewClient(*token, s.Mode == "live" && c.CoordinatorSocket == "")
		if err := client.Connect(cycleCtx); err != nil {
			s.Stage = "blocked"
			readiness = &liveReadiness{At: time.Now(), Reason: err.Error()}
			s.Reason = err.Error()
			update(s)
			return nil
		}
		b := &robinhood.Broker{MCP: client, Config: c}
		if c.CoordinatorSocket != "" {
			coordinated, err := coordinatorClient(c)
			if err != nil {
				return err
			}
			b.SnapshotProvider = func(ctx context.Context) (wheel.Snapshot, error) {
				snapshot, info, err := coordinated.WheelSnapshot(ctx, client, c)
				b.AccountInfo = info
				return snapshot, err
			}
			b.SubmitProvider = coordinated.SubmitOption
		}
		b.SubmitMu = &submitMu
		b.BeforeSubmit = func() error {
			if err := activation.fenceIsolation(); err != nil {
				return err
			}
			if !activation.enabled() || activation.profitOnly() != "" {
				return errors.New("live activation absent or invalid before submission")
			}
			if _, err := os.Lstat(filepath.Join(*stateDir, "HALT")); !errors.Is(err, os.ErrNotExist) {
				return errors.New("HALT present or unreadable before submission")
			}
			return nil
		}
		snapshot, err := b.Snapshot(cycleCtx)
		if b.AccountInfo != nil || c.CoordinatorSocket == "" {
			lastAccountInfo = b.AccountInfo
		}
		if err != nil {
			s.Stage = "blocked"
			readiness = &liveReadiness{At: time.Now(), Reason: err.Error()}
			s.Reason = err.Error()
			update(s)
			return nil
		}
		lastBroker = new(snapshot)
		if fixed != nil {
			blocked := func(cause error) error {
				readiness = &liveReadiness{At: time.Now(), Policy: c.FixedPolicy.Spec.ID, Reason: cause.Error()}
				s.Stage, s.Reason = "blocked", cause.Error()
				update(s)
				if errors.Is(cause, wheel.ErrFixedJournalDurability) {
					return cause
				}
				return nil
			}
			if err = tradePulseIsolation(c, retired); err != nil {
				return blocked(err)
			}
			if err = fixed.BindNative(c, snapshot, time.Now()); err != nil {
				return blocked(err)
			}
			if err = fixed.ReconcileNative(c, snapshot, time.Now()); err != nil {
				return blocked(err)
			}
			if err = fixed.Recover(time.Now()); err != nil {
				return blocked(err)
			}
			if err = fixed.RecoverTakeProfit(time.Now()); err != nil {
				return blocked(err)
			}
			signalAt, executionAt, latestAt, entryDay := wheel.FixedEntryTimes(time.Now())
			closeSignal, closeExecution, closeLatest, closeDay := wheel.FixedCloseTimes(time.Now())
			tp75 := c.FixedPolicy.Spec.ID == wheel.FixedConservativeTP75Policy
			if entryDay && !time.Now().Before(signalAt) && time.Now().Before(executionAt) || tp75 && closeDay && !time.Now().Before(closeSignal) && time.Now().Before(closeExecution) {
				// Reserve both cohorts before parallel source reads. The same
				// minute serves daily closes and the existing weekly entries.
				captureEntry, captureClose := false, false
				if tp75 && (st.Fixed.TakeProfit == nil || st.Fixed.TakeProfit.Day != closeSignal.Format(time.DateOnly)) {
					if err = fixed.BeginTakeProfit(time.Now()); err != nil {
						return blocked(err)
					}
					captureClose = true
				}
				if entryDay && (st.Fixed.Batch == nil || st.Fixed.Batch.Day != signalAt.Format(time.DateOnly)) {
					if err = fixed.Begin(c.FixedPolicy.Roster, time.Now()); err != nil {
						return blocked(err)
					}
					captureEntry = true
				}
				if err = captureFixedCohorts(cycleCtx, fixed, &st, b, captureEntry, captureClose, time.Now); err != nil {
					return blocked(err)
				}
				// Do not spend the original capture minute on compatibility scans.
				s.Stage, s.Reason = "fixed_signal_prepared", "Scheduled source cohorts saved; waiting for their execution clock"
				update(s)
				return nil
			}
			if activation.Capable && (readiness == nil || !readiness.Ready || time.Since(readiness.At) > 20*time.Minute) {
				checked := fixedPreflight(cycleCtx, c, fixed, &st, b, snapshot)
				snapshot, err = b.Snapshot(cycleCtx)
				lastAccountInfo, lastBroker = b.AccountInfo, new(snapshot)
				if err != nil {
					return blocked(err)
				}
				if err = fixed.ReconcileNative(c, snapshot, time.Now()); err != nil {
					return blocked(err)
				}
				readiness = &checked
				if !checked.Ready {
					s.Stage, s.Reason = "blocked", checked.Reason
					update(s)
					return nil
				}
			}
			if tp75 && closeDay && !time.Now().Before(closeExecution) && !time.Now().After(closeLatest) && st.Fixed.TakeProfit != nil {
				if s.Mode != "live" || halted || readiness == nil || !readiness.Ready {
					s.Stage, s.Reason = "halted", "TP signals remain read-only until authenticated native activation"
					update(s)
					return nil
				}
				for _, signal := range st.Fixed.TakeProfit.Signals {
					if signal.Phase != "ready" {
						continue
					}
					if err = fixed.ExecuteTakeProfit(cycleCtx, c, b, signal.Symbol, time.Now); err != nil {
						return blocked(err)
					}
					s.Stage, s.Reason = "submitted", "One TP75 buyback submitted; waiting for broker reconciliation"
					select {
					case wake <- struct{}{}:
					default:
					}
					update(s)
					return nil
				}
			}
			if !entryDay || time.Now().Before(executionAt) || time.Now().After(latestAt) {
				s.Stage, s.Reason = "fixed_holding", "Reconciled own lifecycles; waiting for the next weekly entry clock"
				if tp75 {
					s.Reason = "Reconciled own lifecycles; waiting for the next daily TP75 or weekly entry clock"
				}
				update(s)
				return nil
			}
			if s.Mode != "live" || halted || readiness == nil || !readiness.Ready {
				s.Stage, s.Reason = "halted", "Fixed signals remain read-only until authenticated native activation"
				update(s)
				return nil
			}
			s.Stage = "fixed_holding"
			if st.Fixed.Batch != nil {
				for _, signal := range st.Fixed.Batch.Signals {
					if signal.Phase != "ready" {
						continue
					}
					if err = fixed.ExecuteNative(cycleCtx, c, b, signal.Symbol, time.Now); err != nil {
						return blocked(err)
					}
					s.Stage, s.Reason = "submitted", "One fixed-policy opening submitted; waiting for broker reconciliation"
					select {
					case wake <- struct{}{}:
					default:
					}
					break
				}
			}
			update(s)
			return nil
		}
		if err = wheel.ReconcileForConfig(c, &st, snapshot, time.Now()); err != nil {
			s.Stage = "blocked"
			readiness = &liveReadiness{At: time.Now(), Reason: err.Error()}
			s.Reason = err.Error()
			update(s)
			return nil
		}
		if err = j.Save(st); err != nil {
			return err
		}
		if activation.Capable {
			if err = tradePulseIsolation(c, retired); err != nil {
				s.Stage = "blocked"
				s.Reason = err.Error()
				readiness = &liveReadiness{At: time.Now(), Reason: err.Error()}
				update(s)
				return nil
			}
		}
		if activation.Capable && (readiness == nil || !readiness.Ready || time.Since(readiness.At) > 20*time.Minute) {
			checked := preflight(cycleCtx, c, st, b, snapshot, retired)
			// Publish the actual snapshot again after compatibility scans.
			snapshot, err = b.Snapshot(cycleCtx)
			lastAccountInfo = b.AccountInfo
			if err != nil {
				readiness = &liveReadiness{At: time.Now(), Reason: err.Error()}
				s.Stage = "blocked"
				s.Reason = err.Error()
				update(s)
				return nil
			}
			lastBroker = new(snapshot)
			checked, err = recheckReadiness(c, &st, j, snapshot, checked, time.Now())
			if err != nil {
				return err
			}
			readiness = &checked
			if !checked.Ready {
				s.Stage = "blocked"
				s.Reason = checked.Reason
				update(s)
				return nil
			}
		}
		// Refresh account facts and reconcile outside market hours; scans,
		// reviews and submissions remain inside the configured order window.
		if !wheel.TradingWindow(time.Now()) {
			s.Stage = "outside_trading_window"
			update(s)
			return nil
		}
		liveClose := s.Mode == "live" && !halted
		if handled, candidate, closeErr := wheel.RunPriorityClose(cycleCtx, c, &st, j, b, snapshot, liveClose, time.Now); handled || closeErr != nil {
			s.Stage = "priority_profit_close"
			s.Reason = "Priority profit close precedes all new entries"
			if candidate != nil {
				s.Candidates = []wheel.Candidate{*candidate}
			}
			if closeErr != nil {
				s.Stage, s.Reason = "blocked", closeErr.Error()
			} else if halted {
				s.Stage, s.Reason = "halted", "Priority profit close prepared; live trading is paused"
			} else if liveClose {
				s.Stage, s.Reason = "submitted", "Priority close submitted; entries wait for confirmed fill and released position"
				// Reconcile once promptly after acceptance. Pending or uncertain
				// outcomes then wait for the normal poll; never resubmit the close.
				select {
				case wake <- struct{}{}:
				default:
				}
			}
			update(s)
			return nil
		}
		candidates, scanErrors := scan(cycleCtx, c, st, b, snapshot.AvailableCash)
		if len(scanErrors) == len(c.Symbols) {
			s.Stage = "blocked"
			s.Reason = "all symbol scans failed"
			update(s)
			return nil
		}
		// Refresh the account after slow chain scans; never allocate old cash.
		snapshot, err = b.Snapshot(cycleCtx)
		if err != nil {
			s.Stage = "blocked"
			readiness = &liveReadiness{At: time.Now(), Reason: err.Error()}
			s.Reason = err.Error()
			update(s)
			return nil
		}
		// Publish an immutable copy; the next snapshot must not mutate HTTP readers.
		lastBroker = new(snapshot)
		// Chain scans can outlive a fill or manual trade. Reconcile this newer
		// snapshot before publishing a plan or retaining readiness for activation.
		if err = wheel.ReconcileForConfig(c, &st, snapshot, time.Now()); err != nil {
			s.Stage = "blocked"
			readiness = &liveReadiness{At: time.Now(), Reason: err.Error()}
			s.Reason = err.Error()
			update(s)
			return nil
		}
		if err = j.Save(st); err != nil {
			return err
		}
		plan, err := wheel.Plan(c, st, snapshot, candidates, time.Now())
		if err != nil {
			s.Stage = "blocked"
			readiness = &liveReadiness{At: time.Now(), Reason: err.Error()}
			s.Reason = err.Error()
			update(s)
			return nil
		}
		s.Candidates = plan
		s.Stage = "planned"
		if len(scanErrors) > 0 {
			s.Reason = fmt.Sprintf("%d symbol scans failed; see journal logs", len(scanErrors))
		}
		if halted {
			s.Stage = "halted"
			s.Reason = "Live trading is paused; candidates are read-only"
			update(s)
			return nil
		}
		if s.Mode == "live" {
			for _, a := range plan {
				if _, err := os.Lstat(filepath.Join(*stateDir, "HALT")); !errors.Is(err, os.ErrNotExist) {
					s.Stage = "halted"
					break
				}
				if err = wheel.Execute(cycleCtx, c, &st, j, b, a, time.Now); err != nil {
					s.Stage = "blocked"
					s.Reason = err.Error()
					break
				}
				// One in-flight submission at a time; the next cycle must reconcile it.
				s.Stage = "submitted"
				break
			}
		}
		update(s)
		return nil
	}
	if cmd == "once" {
		if err = cycle(); err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(operatorStatus(current, c, *token, *stateDir))
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil || (host != "127.0.0.1" && host != "::1") {
		return errors.New("status server must listen on loopback")
	}
	if err := ensureOperatorToken(*stateDir); err != nil {
		return errors.New("private operator credential could not be provisioned")
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	readStatus := func() status {
		mu.RLock()
		defer mu.RUnlock()
		s := operatorStatus(current, c, *token, *stateDir)
		if activation.enabled() {
			s.Mode = "live"
		} else {
			s.Mode = "read_only"
		}
		if activation.Capable {
			if err := tradePulseIsolation(c, retired); err != nil {
				s.Mode, s.Stage, s.Reason = "read_only", "blocked", err.Error()
				s.LiveReadiness = &liveReadiness{At: time.Now(), Reason: err.Error()}
			}
		}
		return s
	}
	profit := &profitControl{activation: activation, config: c, token: *token, state: &st, store: j, cycleMu: &cycleMu, submitMu: &submitMu, read: readStatus, wake: wake}
	if c.FixedPolicy != nil {
		profit = nil // The conservative hold policy has no discretionary close route.
	}
	mux := operatorAPI(readStatus, mode, *stateDir, wake, operatorFeatures{Activation: activation, SubmitMu: &submitMu, Profit: profit})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 40 * time.Second, IdleTimeout: 30 * time.Second}
	serverErr := make(chan error, 1)
	go func() { serverErr <- server.Serve(listener) }()
	defer server.Close()
	ticker := time.NewTicker(time.Duration(c.PollSeconds) * time.Second)
	defer ticker.Stop()
	for {
		if err = cycle(); err != nil {
			return err
		}
		poll := ticker.C
		if c.FixedPolicy != nil {
			poll = time.After(cycleDelay(c, time.Now()))
		}
		select {
		case <-ctx.Done():
			return nil
		case err = <-serverErr:
			return err
		case <-poll:
		case <-wake:
		}
	}
}

func loadConfig(path string) (wheel.Config, error) {
	c := wheel.DefaultConfig()
	f, err := aruntime.PrivateRead(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() > 65536 {
		return c, errors.New("configuration exceeds size limit or cannot be inspected")
	}
	d := json.NewDecoder(io.LimitReader(f, 65536))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return c, err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return c, errors.New("configuration has trailing content")
	}
	return c, c.Validate()
}

func scan(ctx context.Context, c wheel.Config, st wheel.State, b wheel.Broker, available wheel.Cents) ([]wheel.Candidate, []error) {
	jobs := make(chan string, len(c.Symbols))
	for _, s := range c.Symbols {
		jobs <- s
	}
	close(jobs)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var out []wheel.Candidate
	var failures []error
	for range min(c.MaxSymbols, len(c.Symbols)) {
		wg.Go(func() {
			for symbol := range jobs {
				kind := "put"
				if st.Positions[symbol].OwnedShares == 100 {
					kind = "call"
				}
				quotes, err := b.Quotes(ctx, symbol, kind)
				if err != nil {
					mu.Lock()
					failures = append(failures, err)
					mu.Unlock()
					slog.Warn("symbol scan blocked", "symbol", symbol, "reason", err.Error())
					continue
				}
				if candidate, ok := wheel.SelectFunded(c, symbol, kind, quotes, st.Positions[symbol].Cost, available, time.Now()); ok {
					mu.Lock()
					out = append(out, candidate)
					mu.Unlock()
				}
			}
		})
	}
	wg.Wait()
	return out, failures
}
