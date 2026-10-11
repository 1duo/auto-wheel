package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"autowheel/internal/databento"
)

const maximumQuoteObservations = 3
const maximumQuoteResponse = 1 << 20

// This wrapper retries only the fixed, free metadata.get_cost endpoint.
// Every other request, including batch.submit_job, goes directly to base once.
type quoteTransport struct {
	base   http.RoundTripper
	record func(quoteObservation) error
	wait   func(context.Context, time.Duration) error
}

type quoteObservation struct {
	Attempt           int       `json:"attempt"`
	ObservedAt        time.Time `json:"observed_at"`
	StatusCode        int       `json:"status_code"`
	ResponseBytes     int       `json:"response_bytes"`
	ResponseSHA256    string    `json:"response_sha256"`
	ErrorClass        string    `json:"error_class"`
	Retryable         bool      `json:"retryable"`
	FinancialRequests int       `json:"financial_requests"`
}

func transientQuoteError(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if network, ok := errors.AsType[net.Error](err); ok {
		return network.Timeout()
	}
	return false
}

func transientQuoteStatus(status int) bool {
	switch status {
	case 429, 500, 502, 503, 504:
		return true
	default:
		return false
	}
}

func waitForQuote(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (t *quoteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodPost || req.URL.String() != databento.BaseURL+databento.CostEndpoint || req.GetBody == nil {
		return t.base.RoundTrip(req)
	}
	defer req.Body.Close()
	wait := t.wait
	if wait == nil {
		wait = waitForQuote
	}
	for index := range maximumQuoteObservations {
		if err := req.Context().Err(); err != nil {
			return nil, err
		}
		attempt := req.Clone(req.Context())
		body, err := req.GetBody()
		if err != nil {
			return nil, errors.New("free quote request body unavailable")
		}
		attempt.Body = body
		response, transportErr := t.base.RoundTrip(attempt)
		observation := quoteObservation{Attempt: index + 1, ObservedAt: time.Now().UTC()}
		var raw []byte
		if response != nil {
			observation.StatusCode = response.StatusCode
			raw, err = io.ReadAll(io.LimitReader(response.Body, maximumQuoteResponse+1))
			err = errors.Join(err, response.Body.Close())
			observation.ResponseBytes = len(raw)
			digest := sha256.Sum256(raw)
			observation.ResponseSHA256 = hex.EncodeToString(digest[:])
			if len(raw) > maximumQuoteResponse {
				observation.ErrorClass = "oversized_response"
				transportErr = errors.New("free quote response exceeds bound")
			} else if err != nil {
				observation.ErrorClass = "response_transport"
				transportErr = err
			}
		}
		if transportErr != nil && observation.ErrorClass == "" {
			observation.ErrorClass = "transport"
		}
		observation.Retryable = req.Context().Err() == nil && ((transportErr != nil && transientQuoteError(transportErr)) ||
			(transportErr == nil && response != nil && transientQuoteStatus(response.StatusCode)))
		if err := t.record(observation); err != nil {
			return nil, errors.New("free quote observation could not be persisted")
		}
		if !observation.Retryable || index+1 == maximumQuoteObservations {
			if transportErr != nil {
				return nil, errors.New("free quote transport stopped")
			}
			if response == nil {
				return nil, errors.New("free quote response missing")
			}
			response.Body = io.NopCloser(bytes.NewReader(raw))
			return response, nil
		}
		if err := wait(req.Context(), time.Duration(2*(index+1))*time.Second); err != nil {
			return nil, err
		}
	}
	panic("unreachable quote observation bound")
}
