package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"autowheel/internal/wheel"
)

func TestOperatorControlBoundary(t *testing.T) {
	dir := t.TempDir()
	wake := make(chan struct{}, 1)
	read := func() status { return operatorStatus(status{Stage: "planned"}, wheel.DefaultConfig(), "missing", dir) }
	request := func(mode, action, origin, header string) int {
		r := httptest.NewRequest(http.MethodPost, "/control/"+action, nil)
		r.Host = "127.0.0.1:19841"
		r.Header.Set("Origin", origin)
		r.Header.Set("X-AutoWheel-Control", header)
		w := httptest.NewRecorder()
		operatorAPI(read, mode, dir, wake).ServeHTTP(w, r)
		return w.Code
	}
	for _, origin := range []string{"https://evil.example", "null"} {
		if got := request("read_only", "halt", origin, "confirm"); got != 403 {
			t.Fatalf("browser control accepted: %d", got)
		}
	}
	if got := request("read_only", "halt", "", ""); got != 403 {
		t.Fatalf("missing confirmation accepted: %d", got)
	}
	if got := request("live", "halt", "", "confirm"); got != 202 || !read().Halted {
		t.Fatal("pause must persist even in live mode")
	}
	info, err := os.Stat(filepath.Join(dir, "HALT"))
	if err != nil || info.Mode().Perm() != 0600 || len(wake) != 1 {
		t.Fatal("HALT must be private and wake the loop")
	}
	for _, action := range []string{"resume", "refresh"} {
		if got := request("live", action, "", "confirm"); got != 409 || !read().Halted {
			t.Fatalf("live %s must remain blocked", action)
		}
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "http://127.0.0.1:9841/readyz", nil)
	operatorAPI(read, "read_only", dir, wake).ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatal("paused daemon cannot report ready")
	}
	if got := request("read_only", "resume", "", "confirm"); got != 202 || read().Halted {
		t.Fatal("read-only resume failed")
	}
	w = httptest.NewRecorder()
	operatorAPI(read, "read_only", dir, wake).ServeHTTP(w, httptest.NewRequest("GET", "http://evil.example/status", nil))
	if w.Code != 403 {
		t.Fatal("DNS rebinding Host must not read private operator state")
	}
}

func TestOperatorStatusPrivacy(t *testing.T) {
	c := wheel.DefaultConfig()
	c.Account = "123456789"
	s := operatorStatus(status{}, c, "missing", t.TempDir())
	if !s.AccountConfigured || s.Config.Account != "" || s.TokenPresent || s.Broker != nil || s.ServerTime.IsZero() {
		t.Fatal("status leaked account or fabricated broker data")
	}
	if c.Account != "123456789" {
		t.Fatal("redaction changed daemon configuration")
	}
}
