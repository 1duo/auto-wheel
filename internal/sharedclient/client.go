// Package sharedclient implements the private, bounded account coordinator API.
package sharedclient

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"autowheel/internal/robinhood"
)

const MaxResponse = 16 << 20

type Request struct {
	Version    int             `json:"version"`
	Op         string          `json:"op"`
	Release    string          `json:"release"`
	PolicyHash string          `json:"policy_hash"`
	Payload    json.RawMessage `json:"payload"`
}

type Response struct {
	OK     bool            `json:"ok"`
	Error  string          `json:"error,omitempty"`
	Unsent bool            `json:"unsent"`
	Data   json.RawMessage `json:"data"`
}

type Denial struct {
	Reason string
	Unsent bool
}

func (e *Denial) Error() string { return e.Reason }

type Evidence struct {
	Schema       string    `json:"schema_version"`
	AccountHash  string    `json:"account_identity_hash"`
	PolicyHash   string    `json:"policy_hash"`
	Release      string    `json:"client_release"`
	FullHash     string    `json:"full_view_hash"`
	OwnedHash    string    `json:"owned_view_hash"`
	At           time.Time `json:"observed_at"`
	Generation   int64     `json:"generation"`
	Epoch        int64     `json:"permission_epoch"`
	Reconciled   bool      `json:"full_account_reconciled"`
	Qualified    bool      `json:"qualified"`
	SingleWriter bool      `json:"writer_exclusive"`
}

type View struct {
	Evidence  Evidence `json:"evidence"`
	FullJSON  string   `json:"full_json"`
	OwnedJSON string   `json:"owned_json"`
}

type Status struct {
	Ready         bool                   `json:"ready"`
	Reason        string                 `json:"reason,omitempty"`
	AccountHash   string                 `json:"account_identity_hash"`
	PolicyHash    string                 `json:"policy_hash"`
	Release       string                 `json:"client_release"`
	At            time.Time              `json:"at"`
	Generation    int64                  `json:"generation"`
	Epoch         int64                  `json:"permission_epoch"`
	NewRisk       bool                   `json:"new_risk"`
	Management    bool                   `json:"management"`
	Reconciled    bool                   `json:"full_account_reconciled"`
	SingleWriter  bool                   `json:"writer_exclusive"`
	AvailableCash int64                  `json:"available_cash_cents"`
	Capital       int64                  `json:"committed_capital_cents"`
	HeldCash      int64                  `json:"reserved_cash_cents"`
	PeerSymbols   []string               `json:"peer_symbols,omitempty"`
	Blockers      []string               `json:"blockers,omitempty"`
	BrokerSummary *robinhood.AccountInfo `json:"broker_summary,omitempty"`
}

type Client struct {
	Socket     string
	Release    string
	PolicyHash string
}

func Digest(raw []byte) string { return fmt.Sprintf("%x", sha256.Sum256(raw)) }

func ExecutableDigest() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("client executable identity unavailable")
	}
	h := sha256.New()
	if _, err = io.Copy(h, file); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func (c Client) Call(ctx context.Context, op string, payload any, out any) error {
	if !filepath.IsAbs(c.Socket) || len(c.Release) != 64 || len(c.PolicyHash) != 64 {
		return errors.New("shared execution binding is incomplete")
	}
	dir, err := os.Lstat(filepath.Dir(c.Socket))
	if err != nil || !dir.IsDir() || dir.Mode().Perm()&0077 != 0 {
		return errors.New("coordinator socket directory is not private")
	}
	info, err := os.Lstat(c.Socket)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm()&0077 != 0 {
		return errors.New("coordinator socket is unavailable or insecure")
	}
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return errors.New("shared execution coordinator unavailable")
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if err = conn.SetDeadline(deadline); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	request := Request{Version: 1, Op: op, Release: c.Release, PolicyHash: c.PolicyHash, Payload: encoded}
	raw, err := json.Marshal(request)
	if err != nil || len(raw) > 1<<20 {
		return errors.New("coordinator request exceeds limit")
	}
	if _, err = conn.Write(append(raw, '\n')); err != nil {
		return err
	}
	// The server closes every connection after one response. LimitReader cannot
	// turn a partial/truncated message into an authoritative empty response.
	d := json.NewDecoder(bufio.NewReader(io.LimitReader(conn, MaxResponse+1)))
	d.DisallowUnknownFields()
	var response Response
	if err = d.Decode(&response); err != nil {
		return errors.New("coordinator response missing or malformed")
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return errors.New("coordinator response has trailing or oversized content")
	}
	if !response.OK {
		if response.Error == "" || len(response.Error) > 512 {
			return errors.New("coordinator rejection is malformed")
		}
		return &Denial{Reason: response.Error, Unsent: response.Unsent}
	}
	if out != nil {
		if len(response.Data) == 0 || string(response.Data) == "null" {
			return errors.New("coordinator response data missing")
		}
		if err = json.Unmarshal(response.Data, out); err != nil {
			return errors.New("coordinator response data invalid")
		}
	}
	return nil
}

func (c Client) ValidateView(view View, accountHash string, now time.Time) error {
	e := view.Evidence
	age := now.Sub(e.At)
	if e.Schema != "SharedAccountEvidenceV1" || e.AccountHash != accountHash || e.PolicyHash != c.PolicyHash || e.Release != c.Release || e.Generation <= 0 || e.Epoch <= 0 || !e.Reconciled || !e.Qualified || !e.SingleWriter || age < -2*time.Second || age > 30*time.Second || Digest([]byte(view.FullJSON)) != e.FullHash || Digest([]byte(view.OwnedJSON)) != e.OwnedHash {
		return errors.New("shared account evidence is stale, mismatched or unqualified")
	}
	return nil
}
