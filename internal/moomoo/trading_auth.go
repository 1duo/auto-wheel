package moomoo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	aruntime "autowheel/internal/runtime"
)

// TradingTokens is deliberately separate from the observation credential. A
// read-only grant never becomes a trading grant by changing a daemon setting.
type TradingTokens struct {
	Path      string
	AccountID string
	mu        sync.Mutex
}

func tradingScopeError(account, scope string) error {
	if !validAccountID(account) {
		return errors.New("trading credential requires the sealed business account")
	}
	want := map[string]bool{"quote:read": false, "trade:read": false, "trade:write": false, "accid:" + account: false}
	for item := range strings.FieldsSeq(scope) {
		seen, exists := want[item]
		if !exists || seen {
			return errors.New("trading grant contains an unsupported, repeated or different-account scope")
		}
		want[item] = true
	}
	for _, present := range want {
		if !present {
			return errors.New("exact account-bound trading permission is missing")
		}
	}
	return nil
}

func (t *TradingTokens) Access(ctx context.Context, h *http.Client) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if ctx.Err() != nil || t.Path == "" || h == nil || !validAccountID(t.AccountID) {
		return "", errors.New("trading credential configuration or context is invalid")
	}
	unlock, err := lockCredential(ctx, t.Path+".lock")
	if err != nil {
		return "", errors.New("trading credential lock unavailable")
	}
	defer unlock()
	f, err := aruntime.PrivateRead(t.Path)
	if err != nil {
		return "", errors.New("separate private Moomoo trading grant is missing")
	}
	var c Credential
	raw, err := readBounded(f, 32768)
	if err == nil {
		err = privateJSON(bytes.NewReader(raw), 32768, &c)
	}
	f.Close()
	canonical, encodeErr := json.MarshalIndent(c, "", "  ")
	if err != nil || encodeErr != nil || !bytes.Equal(raw, append(canonical, '\n')) || c.Resource != Resource || c.ClientID == "" || c.AccessToken == "" || c.RefreshToken == "" || c.ExpiresAt.IsZero() || tradingScopeError(t.AccountID, c.Scope) != nil {
		return "", errors.New("invalid or insufficient account-bound trading grant")
	}
	if time.Until(c.ExpiresAt) > 5*time.Minute {
		return c.AccessToken, nil
	}
	c, err = exchangeWithScopes(ctx, h, url.Values{"grant_type": {"refresh_token"}, "client_id": {c.ClientID}, "refresh_token": {c.RefreshToken}, "resource": {Resource}}, c,
		func(scope string) error { return tradingScopeError(t.AccountID, scope) })
	if err != nil {
		return "", errors.New("trading credential refresh failed; no replay")
	}
	if err := aruntime.AtomicJSON(t.Path, c); err != nil {
		return "", errors.New("trading credential replacement failed; reauthentication required")
	}
	return c.AccessToken, nil
}
