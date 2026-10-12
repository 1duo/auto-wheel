package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCalendarReadOnlyBoundary(t *testing.T) {
	wake := make(chan struct{}, 1)
	api := operatorAPI(func() status { t.Fatal("static calendar read operator state"); return status{} }, "read_only", t.TempDir(), wake)
	for _, path := range []string{"/calendar", "/calendar.js"} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			r := httptest.NewRequest(method, "http://127.0.0.1:19841"+path, nil)
			w := httptest.NewRecorder()
			api.ServeHTTP(w, r)
			if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "connect-src 'self'") {
				t.Fatalf("%s %s: code %d, headers %v", method, path, w.Code, w.Header())
			}
			if method == http.MethodHead && w.Body.Len() != 0 {
				t.Fatal("HEAD returned calendar content")
			}
		}
		for _, request := range []*http.Request{
			httptest.NewRequest(http.MethodPost, "http://127.0.0.1:19841"+path, nil),
			httptest.NewRequest(http.MethodGet, "http://evil.example"+path, nil),
		} {
			w := httptest.NewRecorder()
			api.ServeHTTP(w, request)
			want := http.StatusMethodNotAllowed
			if request.Host == "evil.example" {
				want = http.StatusForbidden
			}
			if w.Code != want {
				t.Fatalf("%s %s: code %d, want %d", request.Method, request.Host, w.Code, want)
			}
		}
	}
	if len(wake) != 0 {
		t.Fatal("calendar woke the trading loop")
	}
}
