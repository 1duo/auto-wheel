package moomoo

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	aruntime "autowheel/internal/runtime"
)

const tokenEndpoint = "https://webapi.moomoo.com/oauth2/token"
const registerEndpoint = "https://webapi.moomoo.com/oauth2/register"
const observerScope = "quote:read trade:read accid:*"

type Credential struct {
	Resource     string    `json:"resource"`
	ClientID     string    `json:"client_id"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	Scope        string    `json:"scope"`
}
type Tokens struct {
	mu   sync.Mutex
	Path string
}

func (t *Tokens) Access(ctx context.Context, h *http.Client) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	unlock, err := lockCredential(ctx, t.Path+".lock")
	if err != nil {
		return "", errors.New("Moomoo credential refresh lock unavailable")
	}
	defer unlock()
	f, err := aruntime.PrivateRead(t.Path)
	if err != nil {
		return "", errors.New("Moomoo token missing or not private; run login")
	}
	defer f.Close()
	var c Credential
	if err = privateJSON(f, 32768, &c); err != nil || c.Resource != Resource || c.ClientID == "" || c.AccessToken == "" || c.RefreshToken == "" || c.ExpiresAt.IsZero() || !readOnlyScopes(c.Scope) {
		return "", errors.New("invalid Moomoo credential; run login")
	}
	if time.Until(c.ExpiresAt) > 5*time.Minute {
		return c.AccessToken, nil
	}
	c, err = exchange(ctx, h, url.Values{"grant_type": {"refresh_token"}, "client_id": {c.ClientID}, "refresh_token": {c.RefreshToken}, "resource": {Resource}}, c)
	if err != nil {
		return "", errors.New("OAuth refresh failed; run login (no automatic refresh replay)")
	}
	if err = aruntime.AtomicJSON(t.Path, c); err != nil {
		return "", errors.New("could not persist refreshed OAuth credential; stop and reauthenticate")
	}
	return c.AccessToken, nil
}

// Lock a separate inode across read/refresh/atomic replacement so token
// refresh cannot race between independent clients of this deployment.
func lockCredential(ctx context.Context, path string) (func(), error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "credential refresh lock")
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		f.Close()
		return nil, errors.New("credential lock is not private")
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	poll := time.NewTicker(25 * time.Millisecond)
	defer poll.Stop()
	for {
		err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-poll.C:
		}
	}
}

func exchange(ctx context.Context, h *http.Client, values url.Values, c Credential) (Credential, error) {
	return exchangeWithScopes(ctx, h, values, c, readOnlyScopeError)
}

func exchangeWithScopes(ctx context.Context, h *http.Client, values url.Values, c Credential, check func(string) error) (Credential, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", tokenEndpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return c, err
	}
	req.GetBody = nil // A refresh grant must never be rewound by HTTP transport.
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := h.Do(req)
	if err != nil {
		return c, errors.New("OAuth transport failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return c, fmt.Errorf("OAuth HTTP %d", resp.StatusCode)
	}
	var result struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		Type    string `json:"token_type"`
		Expires int    `json:"expires_in"`
		Scope   string `json:"scope"`
	}
	if err = boundedJSON(resp.Body, 32768, &result); err != nil || result.Access == "" || !strings.EqualFold(result.Type, "bearer") || result.Expires < 60 || result.Expires > 86400*365 {
		return c, errors.New("invalid OAuth token response")
	}
	if result.Scope != "" {
		c.Scope = result.Scope
	}
	if err := check(c.Scope); err != nil {
		return c, fmt.Errorf("Moomoo grant rejected: %w", err)
	}
	c.AccessToken = result.Access
	if result.Refresh != "" {
		c.RefreshToken = result.Refresh
	}
	if c.RefreshToken == "" {
		return c, errors.New("OAuth refresh token missing")
	}
	c.Resource = Resource
	c.ExpiresAt = time.Now().Add(time.Duration(result.Expires) * time.Second)
	return c, nil
}

// Login uses a loopback callback with PKCE and constant-time state comparison.
// On Linux, forward the chosen callback port over SSH before opening the URL.
func Login(ctx context.Context, path string, port int, show func(string)) error {
	return loginWithScopes(ctx, path, port, observerScope, "AutoWheel Moomoo observer", readOnlyScopeError, readOnlyScopeError, show)
}

// tradingRegistrationScope is the only account-scope form Moomoo registration
// accepts. The consent page binds one account into the issued token, which
// must then name exactly the sealed account or it is rejected and not saved.
const tradingRegistrationScope = "quote:read trade:read trade:write accid:*"

func tradingRegistrationError(scope string) error {
	want := strings.Fields(tradingRegistrationScope)
	got := strings.Fields(scope)
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(want, got) {
		return errors.New("trading client registration scope differs from the reviewed permissions")
	}
	return nil
}

func LoginTrading(ctx context.Context, path string, port int, accountID string, show func(string)) error {
	if !validAccountID(accountID) {
		return errors.New("trading consent requires the sealed business account")
	}
	return loginWithScopes(ctx, path, port, tradingRegistrationScope, "AutoWheel Moomoo native execution", tradingRegistrationError,
		func(scope string) error { return tradingScopeError(accountID, scope) }, show)
}

func loginWithScopes(ctx context.Context, path string, port int, scope, name string, registered, check func(string) error, show func(string)) error {
	h := secureHTTP()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://mcp.moomoo.com/.well-known/oauth-authorization-server", nil)
	resp, err := h.Do(req)
	if err != nil {
		return errors.New("OAuth discovery failed")
	}
	var meta struct {
		Issuer        string `json:"issuer"`
		Authorization string `json:"authorization_endpoint"`
		Token         string `json:"token_endpoint"`
		Register      string `json:"registration_endpoint"`
	}
	err = boundedJSON(resp.Body, 32768, &meta)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 || meta.Issuer != Resource || meta.Authorization != "https://webapi.moomoo.com/oauth2/authorize/confirm" || meta.Token != tokenEndpoint || meta.Register != registerEndpoint {
		return errors.New("Moomoo OAuth discovery does not match trusted endpoints")
	}
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	defer listener.Close()
	redirect := "http://" + listener.Addr().String() + "/oauth/callback"
	clientID, err := registerClient(ctx, h, redirect, scope, name, registered)
	if err != nil {
		return err
	}
	verifier, err := randomSecret()
	if err != nil {
		return err
	}
	state, err := randomSecret()
	if err != nil {
		return err
	}
	hash := sha256.Sum256([]byte(verifier))
	u, _ := url.Parse(meta.Authorization)
	u.RawQuery = url.Values{"client_id": {clientID}, "redirect_uri": {redirect}, "response_type": {"code"}, "scope": {scope}, "resource": {Resource}, "state": {state}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}}.Encode()
	codes := make(chan authorizationResult, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /oauth/callback", authorizationCallback(state, codes))
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	defer server.Close()
	show(u.String())
	select {
	case result := <-codes:
		if result.Err != nil {
			return result.Err
		}
		c, err := exchangeWithScopes(ctx, h, url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {result.Code}, "redirect_uri": {redirect}, "code_verifier": {verifier}, "resource": {Resource}}, Credential{ClientID: clientID}, check)
		if err != nil {
			// Keep only the rejected scope text for private compatibility review.
			// Access/refresh tokens and authorization codes are never retained.
			if c.Scope != "" && check(c.Scope) != nil {
				review := struct {
					At    time.Time `json:"at"`
					Scope string    `json:"scope"`
				}{At: time.Now().UTC(), Scope: c.Scope}
				if saveErr := aruntime.AtomicJSON(path+".scope-review.json", review); saveErr != nil {
					return errors.Join(err, errors.New("could not save private rejected-scope metadata"))
				}
			}
			return err
		}
		unlock, err := lockCredential(ctx, path+".lock")
		if err != nil {
			return errors.New("credential replacement lock unavailable")
		}
		defer unlock()
		return aruntime.AtomicJSON(path, c)
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return errors.New("OAuth callback listener stopped")
	}
}

func registerObserver(ctx context.Context, h *http.Client, redirect string) (string, error) {
	return registerClient(ctx, h, redirect, observerScope, "AutoWheel Moomoo observer", readOnlyScopeError)
}

func registerClient(ctx context.Context, h *http.Client, redirect, scope, name string, check func(string) error) (string, error) {
	// Moomoo otherwise registers every scope by default. Restrict the client
	// itself so its consent page cannot offer write permissions to this observer.
	b, _ := json.Marshal(map[string]any{"client_name": name, "redirect_uris": []string{redirect}, "grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}, "token_endpoint_auth_method": "none", "scope": scope})
	req, _ := http.NewRequestWithContext(ctx, "POST", registerEndpoint, strings.NewReader(string(b)))
	req.GetBody = nil
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.Do(req)
	if err != nil {
		return "", errors.New("OAuth registration failed")
	}
	defer resp.Body.Close()
	var registration struct {
		ClientID string `json:"client_id"`
		Method   string `json:"token_endpoint_auth_method"`
		Scope    string `json:"scope"`
	}
	if err := boundedJSON(resp.Body, 32768, &registration); err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 || registration.ClientID == "" || registration.Method != "none" {
		return "", errors.New("invalid OAuth registration")
	}
	if err := check(registration.Scope); err != nil {
		return "", fmt.Errorf("Moomoo registration rejected: %w", err)
	}
	return registration.ClientID, nil
}

type authorizationResult struct {
	Code string
	Err  error
}

func authorizationCallback(state string, results chan<- authorizationResult) http.HandlerFunc {
	var received atomic.Bool
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if len(r.URL.RawQuery) > 8192 || len(q["state"]) != 1 || len(q["code"]) > 1 || len(q["error"]) > 1 || len(q["iss"]) > 1 ||
			subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 ||
			(q.Get("code") == "") == (q.Get("error") == "") || (q.Get("iss") != "" && q.Get("iss") != Resource) {
			http.Error(w, "OAuth callback rejected", http.StatusBadRequest)
			return
		}
		if !received.CompareAndSwap(false, true) {
			http.Error(w, "Callback already received", http.StatusConflict)
			return
		}
		result := authorizationResult{Code: q.Get("code")}
		message := "AutoWheel authorization received. You can close this tab."
		if q.Get("error") != "" {
			result.Err = errors.New("Moomoo authorization was not completed")
			message = "Moomoo authorization was not granted. You can close this tab."
		}
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprintln(w, message)
		// Deliver the browser acknowledgement before Login can finish the
		// exchange and close its callback server.
		_ = http.NewResponseController(w).Flush()
		results <- result
	}
}

func randomSecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// The observer refuses write-capable credentials even though it has no order API.
func readOnlyScopes(scope string) bool {
	return readOnlyScopeError(scope) == nil
}

// Report only fixed permission categories. Provider scope text can contain
// private account identifiers and must never be included in an error or log.
func readOnlyScopeError(scope string) error {
	quotes, trades := false, false
	for item := range strings.FieldsSeq(scope) {
		switch item {
		case "quote:read":
			quotes = true
		case "trade:read":
			trades = true
		case "quote:write":
			return errors.New("watchlist write permission was granted")
		case "trade:write":
			return errors.New("trading write permission was granted")
		default:
			if !strings.HasPrefix(item, "accid:") {
				return errors.New("an unsupported permission was granted")
			}
			id := strings.TrimPrefix(item, "accid:")
			if id != "*" && !validAccountID(id) {
				return errors.New("account scope encoding is not qualified")
			}
		}
	}
	if !quotes {
		return errors.New("market data read permission is missing")
	}
	if !trades {
		return errors.New("account and trade read permission is missing")
	}
	return nil
}
