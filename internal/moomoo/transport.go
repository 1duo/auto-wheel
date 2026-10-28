package moomoo

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"autowheel/internal/wheel"
)

type TradingClient struct {
	http      *http.Client
	tokens    *TradingTokens
	accountID string
}

func NewTradingClient(tokenPath, accountID string) *TradingClient {
	return &TradingClient{http: secureHTTP(), tokens: &TradingTokens{Path: tokenPath, AccountID: accountID}, accountID: accountID}
}

func (c *TradingClient) CheckAccess(ctx context.Context) error {
	if c == nil || c.tokens == nil || c.http == nil {
		return errors.New("Moomoo trading transport is not configured")
	}
	_, err := c.tokens.Access(ctx, c.http)
	return err
}

func validatePlacementRequest(account string, request PlacementRequest) bool {
	if !validAccountID(account) || request.Path != "/api/v1.0/accounts/"+account+"/orders" {
		return false
	}
	var fields map[string]string
	if exactJSON(request.Body, &fields) != nil || len(fields) != 7 || fields["qty"] != "1" || fields["order_type"] != "LIMIT" {
		return false
	}
	price, err := wheel.ParseCents(fields["price"])
	if err != nil {
		return false
	}
	terms := OrderIntentTerms{AccountID: account, Ref: fields["remark"], Code: fields["code"], Side: fields["side"], Quantity: 1,
		LimitCents: price, TimeInForce: fields["time_in_force"], CreatedAt: time.Now().UTC()}
	encoded, err := PreparePlacement(terms, time.Now().UTC())
	return err == nil && bytes.Equal(encoded.Body, request.Body)
}

// The durable attempt fence invokes this once. A guard is checked after token
// refresh and immediately before Do. Response evidence belongs in private state.
// errTransportNotInvoked is returned only by this transport, and only before
// http.Do. It lets the attempt fence record a provably unsent order; any other
// dispatcher error, including ErrNotSubmitted, remains an unknown outcome.
var errTransportNotInvoked = errors.Join(wheel.ErrNotSubmitted, errors.New("Moomoo order transport was not invoked"))

// placementSendTimeout bounds the single POST. The request is detached from
// cycle cancellation so shutdown or a cycle deadline cannot abort it midway
// and turn a sent order into an unknown outcome.
const placementSendTimeout = 15 * time.Second

func (c *TradingClient) sendPlacement(ctx context.Context, request PlacementRequest, finalGuard func() error) (int, []byte, error) {
	if c == nil || c.tokens == nil || c.http == nil || finalGuard == nil || ctx.Err() != nil || !validatePlacementRequest(c.accountID, request) {
		return 0, nil, errTransportNotInvoked
	}
	token, err := c.tokens.Access(ctx, c.http)
	if err != nil {
		return 0, nil, errors.Join(errTransportNotInvoked, err)
	}
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), placementSendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(sendCtx, http.MethodPost, restEndpoint+request.Path, bytes.NewReader(request.Body))
	if err != nil {
		return 0, nil, errTransportNotInvoked
	}
	req.GetBody = nil
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	if ctx.Err() != nil || finalGuard() != nil {
		return 0, nil, errTransportNotInvoked
	}
	response, err := c.http.Do(req)
	if err != nil {
		return 0, nil, ErrPlacementOutcomeUnknown
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxPlacementResponse+1))
	if err != nil || response.StatusCode != http.StatusOK {
		return response.StatusCode, raw, ErrPlacementOutcomeUnknown
	}
	return response.StatusCode, raw, nil
}
