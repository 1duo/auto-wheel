// Package moomoo connects the independent US deployment to official Moomoo
// interfaces. The MCP client exposes discovery only; native execution uses
// its separately scoped, account-bound REST transport.
package moomoo

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const Endpoint = "https://mcp.moomoo.com/mcp"
const Resource = "https://mcp.moomoo.com"
const protocol = "2025-06-18"

type Tool struct {
	Name         string         `json:"name"`
	Description  string         `json:"description,omitempty"`
	InputSchema  map[string]any `json:"inputSchema"`
	OutputSchema map[string]any `json:"outputSchema,omitempty"`
	Annotations  map[string]any `json:"annotations,omitempty"`
}

type Client struct {
	mu       sync.Mutex
	http     *http.Client
	tokens   *Tokens
	endpoint string
	session  string
	protocol string
	id       int64
	tools    []Tool
}

func secureHTTP() *http.Client {
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, Protocols: protocols,
		DisableKeepAlives: true, TLSHandshakeTimeout: 10 * time.Second}
	return &http.Client{Transport: transport, Timeout: 25 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func NewClient(tokenFile string) *Client {
	return &Client{http: secureHTTP(), tokens: &Tokens{Path: tokenFile}, endpoint: Endpoint}
}

// Connect inventories the provider's actual schemas. It cannot invoke a tool,
// even when the provider advertises financial capabilities in this inventory.
func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tools, c.session, c.protocol = nil, "", protocol
	raw, err := c.rpc(ctx, "initialize", map[string]any{"protocolVersion": protocol, "capabilities": map[string]any{},
		"clientInfo": map[string]string{"name": "AutoWheel-Moomoo-observer", "version": "0.1.0"}}, false)
	if err != nil {
		return err
	}
	var initialized struct {
		Protocol string `json:"protocolVersion"`
	}
	if exactJSON(raw, &initialized) != nil || !slices.Contains([]string{protocol, "2025-03-26"}, initialized.Protocol) {
		return errors.New("Moomoo MCP protocol negotiation failed")
	}
	// Both supported versions use the same initialization, inventory and
	// single-message Streamable HTTP subset exposed by this observer.
	c.protocol = initialized.Protocol
	if _, err := c.rpc(ctx, "notifications/initialized", nil, true); err != nil {
		return err
	}
	var tools []Tool
	seenTools, seenCursors := map[string]bool{}, map[string]bool{}
	cursor, inventoryBytes := "", 0
	for range 64 {
		params := map[string]string{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.rpc(ctx, "tools/list", params, false)
		if err != nil {
			return err
		}
		inventoryBytes += len(raw)
		if inventoryBytes > 16<<20 {
			return errors.New("Moomoo MCP inventory exceeds total size limit")
		}
		var page struct {
			Tools  []Tool `json:"tools"`
			Cursor string `json:"nextCursor"`
		}
		if exactJSON(raw, &page) != nil || page.Tools == nil || len(tools)+len(page.Tools) > 4096 {
			return errors.New("Moomoo MCP inventory missing or exceeds limit")
		}
		for _, tool := range page.Tools {
			if tool.Name == "" || len(tool.Name) > 256 || seenTools[tool.Name] || tool.InputSchema == nil {
				return errors.New("Moomoo MCP tool identity/schema missing or duplicated")
			}
			seenTools[tool.Name] = true
			tools = append(tools, tool)
		}
		if page.Cursor == "" {
			if len(tools) == 0 {
				return errors.New("Moomoo MCP returned no tools")
			}
			slices.SortFunc(tools, func(a, b Tool) int { return strings.Compare(a.Name, b.Name) })
			c.tools = tools
			return nil
		}
		if seenCursors[page.Cursor] {
			return errors.New("Moomoo MCP inventory cursor repeated")
		}
		seenCursors[page.Cursor], cursor = true, page.Cursor
	}
	return errors.New("Moomoo MCP inventory pagination exceeds limit")
}

func (c *Client) Inventory() []Tool {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Deep-copy provider maps so callers cannot mutate the retained inventory.
	raw, _ := json.Marshal(c.tools)
	var result []Tool
	_ = exactJSON(raw, &result)
	return result
}

func (c *Client) rpc(ctx context.Context, method string, params any, notification bool) (json.RawMessage, error) {
	// This allowlist is independent of names/annotations supplied by the server.
	if method != "initialize" && method != "notifications/initialized" && method != "tools/list" {
		return nil, errors.New("Moomoo observer blocks tool invocation and mutations")
	}
	c.id++
	id := c.id
	body := map[string]any{"jsonrpc": "2.0", "method": method}
	if !notification {
		body["id"] = id
	}
	if params != nil {
		body["params"] = params
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, errors.New("invalid Moomoo MCP request")
	}
	token, err := c.tokens.Access(ctx, c.http)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, errors.New("invalid Moomoo MCP endpoint")
	}
	req.GetBody = nil
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	requestProtocol := c.protocol
	if requestProtocol == "" {
		requestProtocol = protocol
	}
	req.Header.Set("MCP-Protocol-Version", requestProtocol)
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, errors.New("Moomoo MCP transport failed; no automatic replay")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Moomoo MCP HTTP %d; no automatic replay", resp.StatusCode)
	}
	if notification {
		if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNoContent {
			return nil, errors.New("Moomoo MCP notification was not acknowledged")
		}
		return nil, nil
	}
	if method == "initialize" {
		c.session = resp.Header.Get("Mcp-Session-Id")
	}
	contentType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return nil, errors.New("Moomoo MCP response media type missing")
	}
	if contentType == "text/event-stream" {
		raw, err = sseResponse(resp.Body, id)
	} else if contentType == "application/json" {
		raw, err = readBounded(resp.Body, 8<<20)
	} else {
		return nil, errors.New("unsupported Moomoo MCP response media type")
	}
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Version string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   json.RawMessage `json:"error"`
		Result  json.RawMessage `json:"result"`
	}
	if exactJSON(raw, &envelope) != nil || envelope.Version != "2.0" || string(envelope.ID) != strconv.FormatInt(id, 10) || len(envelope.Error) != 0 && string(envelope.Error) != "null" || len(envelope.Result) == 0 || string(envelope.Result) == "null" {
		return nil, errors.New("Moomoo MCP result missing, failed or identity mismatched")
	}
	return envelope.Result, nil
}

func sseResponse(r io.Reader, id int64) ([]byte, error) {
	const limit = 8 << 20
	limited := &io.LimitedReader{R: r, N: limit + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), limit+1)
	var data []string
	flush := func() ([]byte, error) {
		if len(data) == 0 {
			return nil, nil
		}
		raw := []byte(strings.Join(data, "\n"))
		data = nil
		var message struct {
			Version string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
		}
		if exactJSON(raw, &message) != nil || message.Version != "2.0" {
			return nil, errors.New("invalid Moomoo MCP SSE message")
		}
		if message.Method != "" && len(message.ID) == 0 {
			return nil, nil
		} // Informational notification; never invoke a server request.
		if message.Method != "" || string(message.ID) != strconv.FormatInt(id, 10) {
			return nil, errors.New("unsupported or mismatched Moomoo MCP SSE message")
		}
		return raw, nil
	}
	for scanner.Scan() {
		if limited.N == 0 {
			return nil, errors.New("Moomoo MCP SSE exceeds limit")
		}
		line := scanner.Text()
		if line == "" {
			if result, err := flush(); err != nil || result != nil {
				return result, err
			}
		} else if value, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimPrefix(value, " "))
		}
	}
	if scanner.Err() != nil || limited.N == 0 {
		return nil, errors.New("Moomoo MCP SSE truncated or exceeds limit")
	}
	if result, err := flush(); err != nil || result != nil {
		return result, err
	}
	return nil, errors.New("Moomoo MCP SSE response missing")
}
