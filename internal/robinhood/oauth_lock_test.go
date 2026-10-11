package robinhood

import (
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	aruntime "autowheel/internal/runtime"
)

type refreshTransport struct{ calls atomic.Int32 }

func (r *refreshTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.calls.Add(1)
	time.Sleep(40 * time.Millisecond)
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"access_token":"new-access","refresh_token":"new-refresh","token_type":"bearer","expires_in":3600}`)), Header: make(http.Header), Request: req}, nil
}
func TestIndependentClientsSerializeCredentialRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	if err := aruntime.AtomicJSON(path, Credential{Resource: Endpoint, ClientID: "fixture", AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	transport := new(refreshTransport)
	httpClient := &http.Client{Transport: transport}
	var wg sync.WaitGroup
	results := make(chan string, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			token, err := (&Tokens{Path: path}).Access(t.Context(), httpClient)
			results <- token
			errs <- err
		})
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for token := range results {
		if token != "new-access" {
			t.Fatal("stale credential after rotation")
		}
	}
	if transport.calls.Load() != 1 {
		t.Fatal("OAuth grant refreshed concurrently", transport.calls.Load())
	}
}
