package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"autowheel/internal/databento"
)

type testRoundTripper func(*http.Request) (*http.Response, error)

func (f testRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type timeoutError struct{}

func (timeoutError) Error() string   { return "test timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestQuoteTransportRecoveryControls(t *testing.T) {
	for _, status := range []int{429, 500, 502, 503, 504} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls, records, waits := 0, 0, 0
			transport := quoteTransport{
				base: testRoundTripper(func(r *http.Request) (*http.Response, error) {
					calls++
					raw, err := io.ReadAll(r.Body)
					r.Body.Close()
					if err != nil || string(raw) != "dataset=OPRA.PILLAR&mode=historical" {
						t.Fatal("quote request changed")
					}
					code := status
					if calls == 3 {
						code = 200
					}
					return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader("1.25"))}, nil
				}),
				record: func(o quoteObservation) error {
					records++
					if o.Attempt != records || o.FinancialRequests != 0 || len(o.ResponseSHA256) != 64 || o.ResponseBytes != 4 {
						t.Fatal("invalid private quote observation")
					}
					return nil
				},
				wait: func(context.Context, time.Duration) error { waits++; return nil },
			}
			r, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, databento.BaseURL+databento.CostEndpoint, strings.NewReader("dataset=OPRA.PILLAR&mode=historical"))
			response, err := transport.RoundTrip(r)
			if err != nil || response.StatusCode != 200 || calls != 3 || records != 3 || waits != 2 {
				t.Fatal("transient quote recovery failed")
			}
			response.Body.Close()
		})
	}
	for _, tc := range []struct {
		name       string
		endpoint   string
		status     int
		err        error
		wantCalls  int
		wantRecord int
	}{
		{"permanent_status", databento.CostEndpoint, 401, nil, 1, 1},
		{"invalid_success_not_retried", databento.CostEndpoint, 200, nil, 1, 1},
		{"permanent_transport", databento.CostEndpoint, 0, errors.New("TLS verification failed"), 1, 1},
		{"EOF_bounded", databento.CostEndpoint, 0, io.EOF, 3, 3},
		{"timeout_bounded", databento.CostEndpoint, 0, timeoutError{}, 3, 3},
		{"submit_never_retried", databento.BatchSubmitEndpoint, 504, nil, 1, 0},
		{"submit_transport_never_retried", databento.BatchSubmitEndpoint, 0, io.EOF, 1, 0},
		{"range_never_retried", databento.RangeEndpoint, 503, nil, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, records := 0, 0
			transport := quoteTransport{
				base: testRoundTripper(func(r *http.Request) (*http.Response, error) {
					calls++
					r.Body.Close()
					if tc.err != nil {
						return nil, tc.err
					}
					return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader("malformed"))}, nil
				}),
				record: func(quoteObservation) error { records++; return nil },
				wait:   func(context.Context, time.Duration) error { return nil },
			}
			r, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, databento.BaseURL+tc.endpoint, strings.NewReader("query"))
			response, _ := transport.RoundTrip(r)
			if response != nil {
				response.Body.Close()
			}
			if calls != tc.wantCalls || records != tc.wantRecord {
				t.Fatalf("calls=%d records=%d", calls, records)
			}
		})
	}
	for _, name := range []string{"cancelled_context", "cancelled_backoff", "persist_failure", "oversized_response"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			transport := quoteTransport{
				base: testRoundTripper(func(r *http.Request) (*http.Response, error) {
					calls++
					r.Body.Close()
					body := "busy"
					if name == "oversized_response" {
						body = strings.Repeat("x", maximumQuoteResponse+1)
					}
					return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader(body))}, nil
				}),
				record: func(quoteObservation) error {
					if name == "persist_failure" {
						return errors.New("disk full")
					}
					return nil
				},
				wait: func(context.Context, time.Duration) error { cancel(); return ctx.Err() },
			}
			want := 1
			if name == "cancelled_context" {
				cancel()
				want = 0
			}
			r, _ := http.NewRequestWithContext(ctx, http.MethodPost, databento.BaseURL+databento.CostEndpoint, strings.NewReader("query"))
			if _, err := transport.RoundTrip(r); err == nil || calls != want {
				t.Fatal("fail-closed quote guard failed")
			}
		})
	}
}
