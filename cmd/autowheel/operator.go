package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"autowheel/internal/calendarui"
	aruntime "autowheel/internal/runtime"
	"autowheel/internal/wheel"
)

// Account numbers and credentials never leave the daemon through its operator API.
func operatorStatus(s status, c wheel.Config, token, dir string) status {
	s.AccountConfigured = c.Account != ""
	s.Config = c.PublicCopy()
	info, err := os.Lstat(token)
	s.TokenPresent = err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0077 == 0
	_, err = os.Lstat(filepath.Join(dir, "HALT"))
	s.Halted = !errors.Is(err, os.ErrNotExist) // Unreadable also fails closed.
	s.ServerTime = time.Now()
	s.TradingWindowOpen = wheel.TradingWindow(s.ServerTime)
	return s
}

type operatorFeatures struct {
	Profit     *profitControl
	Activation *activationControl
	SubmitMu   *sync.Mutex
}

func operatorAPI(read func() status, mode, dir string, wake chan<- struct{}, features ...operatorFeatures) http.Handler {
	var pending atomic.Pointer[selectionOverride]
	readCurrent := read
	read = func() status {
		s := readCurrent()
		if requested := pending.Load(); requested != nil {
			s.PendingSelectionMode, s.PendingStrategyPreset = requested.Mode, requested.Preset
			s.Mode, s.Stage, s.Halted = "read_only", "selection_restart_required", true
			s.Reason = "Selection change saved or unconfirmed; restart the daemon and verify the loaded mode"
			s.LiveReadiness, s.Candidates = nil, nil
		}
		return s
	}
	mux := http.NewServeMux()
	calendarui.Register(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"alive":true}`)
	})
	serveStatus := func(w http.ResponseWriter, r *http.Request) {
		s := read()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path == "/readyz" {
			if (s.LiveCapable && !activationReady(s)) || (!s.LiveCapable && (s.Halted || (s.Stage != "planned" && s.Stage != "submitted"))) {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
		}
		json.NewEncoder(w).Encode(s)
	}
	mux.HandleFunc("GET /status", serveStatus)
	mux.HandleFunc("GET /readyz", serveStatus)
	if len(features) == 1 && features[0].Profit != nil {
		features[0].Profit.read = read
		features[0].Profit.register(mux, dir)
	}
	var localControls sync.Mutex
	controls := &localControls
	var activation *activationControl
	if len(features) == 1 {
		activation = features[0].Activation
		if features[0].SubmitMu != nil {
			controls = features[0].SubmitMu
		}
	}
	changeSelection := func(w http.ResponseWriter, r *http.Request) {
		controls.Lock()
		defer controls.Unlock()
		if !nativeControlRequest(r, dir) {
			http.Error(w, "native operator request required", http.StatusForbidden)
			return
		}
		if len(features) == 1 && features[0].Profit != nil && features[0].Profit.busy.Load() {
			http.Error(w, "close action in progress", http.StatusConflict)
			return
		}
		if activation == nil {
			http.Error(w, "selection control unavailable", http.StatusConflict)
			return
		}
		requested := selectionOverride{Mode: wheel.SelectionMode(r.PathValue("mode")), Preset: r.PathValue("preset")}
		if _, err := requested.apply(activation.Config); err != nil {
			http.Error(w, "unknown selection mode or strategy preset", http.StatusBadRequest)
			return
		}
		// Persist the submission fence before changing policy. This lock is also
		// held by the broker immediately before sending a financial request.
		if err := pauseForSelection(activation, dir); err != nil {
			http.Error(w, "pause could not be fully acknowledged; selection unchanged", http.StatusInternalServerError)
			return
		}
		// A failed directory sync can follow a successful rename. Keep activation
		// fenced until restart even when persistence has an uncertain outcome.
		pending.Store(new(requested))
		if err := aruntime.AtomicJSON(filepath.Join(dir, "selection.json"), requested); err != nil {
			http.Error(w, "selection persistence unconfirmed; restart and verify", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		slog.Info("operator control accepted", "selection_mode", requested.Mode, "strategy_preset", requested.Preset)
	}
	mux.HandleFunc("POST /control/selection/{mode}", changeSelection)
	mux.HandleFunc("POST /control/strategy/{preset}", changeSelection)
	for _, action := range []string{"halt", "resume", "refresh", "live"} {
		mux.HandleFunc("POST /control/"+action, func(w http.ResponseWriter, r *http.Request) {
			controls.Lock()
			defer controls.Unlock()
			if !nativeControlRequest(r, dir) {
				http.Error(w, "native operator request required", http.StatusForbidden)
				return
			}
			if action != "halt" && len(features) == 1 && features[0].Profit != nil && features[0].Profit.busy.Load() {
				http.Error(w, "close action in progress", http.StatusConflict)
				return
			}
			if pending.Load() != nil && action != "halt" {
				http.Error(w, "restart required after selection change", http.StatusConflict)
				return
			}
			if activation == nil && ((mode != "read_only" && action != "halt") || action == "live") {
				http.Error(w, "resume and manual scanning require read-only mode", http.StatusConflict)
				return
			}
			if action == "live" {
				s := read()
				if activation == nil || !activation.Capable || !activationReady(s) {
					http.Error(w, "live readiness checks must pass before activation", http.StatusConflict)
					return
				}
				if err := tradePulseIsolation(activation.Config, activation.RetiredPeer); err != nil {
					http.Error(w, err.Error(), http.StatusConflict)
					return
				}
			}
			if activation != nil && activation.enabled() && (action == "resume" || action == "refresh") {
				http.Error(w, "use the Live Trading switch to resume", http.StatusConflict)
				return
			}
			path := filepath.Join(dir, "HALT")
			var err error
			switch action {
			case "halt":
				err = pauseTrading(activation, dir, "operator pause")
			case "live":
				err = activation.enable()
				if err == nil {
					err = removeControl(dir, "HALT")
				}
			case "resume":
				err = os.Remove(path)
				if errors.Is(err, os.ErrNotExist) {
					err = nil
				}
				if err == nil {
					var f *os.File
					f, err = os.Open(dir)
					if err == nil {
						err = errors.Join(f.Sync(), f.Close())
					}
				}
			}
			if err != nil {
				http.Error(w, "control could not be persisted", http.StatusInternalServerError)
				return
			}
			select {
			case wake <- struct{}{}:
			default:
			}
			w.WriteHeader(http.StatusAccepted)
			slog.Info("operator control accepted", "action", action)
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		// Block browser DNS rebinding from reading the private operator snapshot.
		if host != "127.0.0.1" && host != "::1" {
			http.Error(w, "loopback Host required", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func pauseTrading(activation *activationControl, dir, reason string) error {
	err := persistPause(dir, reason)
	if activation != nil && activation.Config.CoordinatorSocket != "" {
		_, pauseErr := sharedPermission(activation.Config, false)
		err = errors.Join(err, pauseErr)
	}
	return err
}

func persistPause(dir, reason string) error {
	err := aruntime.AtomicJSON(filepath.Join(dir, "HALT"), map[string]string{"reason": reason, "at": time.Now().UTC().Format(time.RFC3339Nano)})
	if err == nil {
		err = removeControl(dir, "LIVE.json")
	}
	return err
}
