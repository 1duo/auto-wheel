package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func authorizeOperatorRequest(t *testing.T, r *http.Request, dir string) {
	t.Helper()
	if err := ensureOperatorToken(dir); err != nil {
		t.Fatal(err)
	}
	token, err := readOperatorToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+token)
}

func TestOperatorAuthenticationFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if err := ensureOperatorToken(dir); err != nil {
		t.Fatal(err)
	}
	wake := make(chan struct{}, 1)
	h := operatorAPI(func() status { return status{Stage: "planned"} }, "read_only", dir, wake)
	for _, action := range []string{"live", "halt", "resume", "refresh", "selection/conservative", "strategy/liquid-roi-v1"} {
		for _, authorization := range []string{"", "Bearer confirm", "Bearer " + strings.Repeat("0", 64)} {
			r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9841/control/"+action, nil)
			r.Header.Set("X-AutoWheel-Control", "confirm")
			r.Header.Set("Authorization", authorization)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Fatalf("unauthenticated %s accepted: %d", action, w.Code)
			}
		}
	}
	for _, name := range []string{"HALT", "LIVE.json", "selection.json"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatal("unauthenticated request changed control state", name)
		}
	}
	if len(wake) != 0 {
		t.Fatal("unauthenticated request woke the trading loop")
	}
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9841/control/refresh", nil)
	r.Header.Set("X-AutoWheel-Control", "confirm")
	authorizeOperatorRequest(t, r, dir)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusAccepted || len(wake) != 1 {
		t.Fatal("authenticated refresh failed", w.Code)
	}
	// Rotation applies immediately even to an existing handler and request.
	if err := os.Remove(filepath.Join(dir, operatorTokenFile)); err != nil {
		t.Fatal(err)
	}
	if nativeControlRequest(r, dir) {
		t.Fatal("removed credential still authorized control")
	}
	if err := ensureOperatorToken(dir); err != nil {
		t.Fatal(err)
	}
	if nativeControlRequest(r, dir) {
		t.Fatal("old credential survived rotation")
	}
	authorizeOperatorRequest(t, r, dir)
	if !nativeControlRequest(r, dir) {
		t.Fatal("rotated credential was not recognized")
	}
	r.TransferEncoding = []string{"chunked"}
	if nativeControlRequest(r, dir) {
		t.Fatal("chunked control body admitted")
	}
}

func TestOperatorCredentialPrivateAndPersistent(t *testing.T) {
	dir := t.TempDir()
	if err := ensureOperatorToken(dir); err != nil {
		t.Fatal(err)
	}
	first, err := readOperatorToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureOperatorToken(dir); err != nil {
		t.Fatal(err)
	}
	second, err := readOperatorToken(dir)
	info, statErr := os.Stat(filepath.Join(dir, operatorTokenFile))
	if err != nil || statErr != nil || first != second || info.Mode().Perm() != 0600 {
		t.Fatal("credential changed on restart or lost private permissions")
	}
	for _, raw := range []string{`null`, `"confirm"`, `"` + first + `" {}`, `"` + first + `"` + strings.Repeat(" ", 4096)} {
		if err := os.WriteFile(filepath.Join(dir, operatorTokenFile), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if ensureOperatorToken(dir) == nil {
			t.Fatal("malformed credential accepted or silently replaced")
		}
	}
	if err := os.WriteFile(filepath.Join(dir, operatorTokenFile), []byte(`"`+first+`"`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, operatorTokenFile), 0644); err != nil {
		t.Fatal(err)
	}
	if ensureOperatorToken(dir) == nil {
		t.Fatal("public credential accepted")
	}
	linkDir := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, operatorTokenFile), filepath.Join(linkDir, operatorTokenFile)); err != nil {
		t.Fatal(err)
	}
	if ensureOperatorToken(linkDir) == nil {
		t.Fatal("symlink credential accepted")
	}
}
