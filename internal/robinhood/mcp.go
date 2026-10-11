// Package robinhood connects only to the official Robinhood trading MCP.
package robinhood

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

const Endpoint = "https://agent.robinhood.com/mcp/trading"
const protocol = "2025-06-18"
const maxResponse = 16 << 20

type object = map[string]any
type Tool struct {
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`
	InputSchema  object `json:"inputSchema"`
	OutputSchema object `json:"outputSchema,omitempty"`
}
type Client struct {
	mu       sync.Mutex
	http     *http.Client
	tokens   *Tokens
	endpoint string
	session  string
	id       int
	tools    map[string]Tool
	live     bool
	// coordinator is restricted to the account service. Strategy clients never
	// gain stock transport permission through NewClient.
	coordinator bool
}

func secureHTTP() *http.Client {
	return &http.Client{Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
func NewClient(tokenFile string, live bool) *Client {
	return &Client{http: secureHTTP(), tokens: &Tokens{Path: tokenFile}, endpoint: Endpoint, live: live}
}

// NewCoordinatorClient permits the three reviewed financial operations owned
// by the shared execution service. Its caller must hold the durable write fence.
// Passing false keeps observation deployments incapable of financial writes.
func NewCoordinatorClient(tokenFile string, live bool) *Client {
	c := NewClient(tokenFile, live)
	c.coordinator = true
	return c
}

func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.session = ""
	c.tools = nil
	v, err := c.rpc(ctx, "initialize", object{"protocolVersion": protocol, "capabilities": object{}, "clientInfo": object{"name": "AutoWheel", "version": "0.1.0"}}, false)
	if err != nil {
		return err
	}
	o, ok := v.(object)
	if !ok || o["protocolVersion"] != protocol {
		return errors.New("MCP protocol negotiation failed")
	}
	if _, err = c.rpc(ctx, "notifications/initialized", nil, true); err != nil {
		return err
	}
	c.tools = map[string]Tool{}
	cursor := ""
	for range 32 {
		params := object{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		v, err = c.rpc(ctx, "tools/list", params, false)
		if err != nil {
			return err
		}
		o, ok = v.(object)
		if !ok {
			return errors.New("invalid MCP tools inventory")
		}
		b, _ := json.Marshal(o["tools"])
		var batch []Tool
		if err = json.Unmarshal(b, &batch); err != nil || len(batch) == 0 {
			return errors.New("empty or invalid MCP tools inventory")
		}
		for _, t := range batch {
			if _, ok := c.tools[t.Name]; ok {
				return errors.New("duplicate MCP tool")
			}
			c.tools[t.Name] = t
		}
		cursor, _ = o["nextCursor"].(string)
		if cursor == "" {
			return nil
		}
	}
	return errors.New("MCP tool inventory exceeds page limit")
}

func (c *Client) Inventory() []Tool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Collect(maps.Values(c.tools))
}

func (c *Client) Supports(name, key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.tools[name]
	if key == "" {
		return ok
	}
	p, _ := t.InputSchema["properties"].(object)
	_, ok = p[key]
	return ok
}

func (c *Client) Call(ctx context.Context, name string, args object) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	stockReview := c.coordinator && name == "review_equity_order"
	stockWrite := c.coordinator && (name == "place_equity_order" || name == "cancel_equity_order")
	if !strings.HasPrefix(name, "get_") && name != "review_option_order" && !stockReview && !(c.live && (name == "place_option_order" || stockWrite)) {
		return nil, errors.New("MCP tool blocked by read-only boundary")
	}
	t, ok := c.tools[name]
	if !ok {
		return nil, fmt.Errorf("MCP lacks required tool %s", name)
	}
	props, _ := t.InputSchema["properties"].(object)
	for k, v := range args {
		s, ok := props[k].(object)
		if !ok {
			return nil, fmt.Errorf("%s input schema lacks %s", name, k)
		}
		if err := validateValue(s, v); err != nil {
			return nil, fmt.Errorf("%s.%s: %w", name, k, err)
		}
	}
	if required, ok := t.InputSchema["required"].([]any); ok {
		for _, r := range required {
			k, _ := r.(string)
			if _, ok := args[k]; !ok {
				return nil, fmt.Errorf("%s requires %s", name, k)
			}
		}
	}
	v, err := c.rpc(ctx, "tools/call", object{"name": name, "arguments": args}, false)
	if err != nil {
		return nil, err
	}
	o, ok := v.(object)
	if !ok || o["isError"] == true {
		return nil, fmt.Errorf("%s returned an application error", name)
	}
	if s, ok := o["structuredContent"]; ok && s != nil {
		return s, nil
	}
	blocks, ok := o["content"].([]any)
	if !ok || len(blocks) != 1 {
		return nil, errors.New("ambiguous MCP tool content")
	}
	b, ok := blocks[0].(object)
	if !ok || b["type"] != "text" {
		return nil, errors.New("MCP tool content is not JSON text")
	}
	s, ok := b["text"].(string)
	if !ok {
		return nil, errors.New("missing tool JSON text")
	}
	return decode([]byte(s))
}

func validateValue(schema object, v any) error {
	// Unknown schemas fail closed rather than coercing money/order units.
	if types, ok := schema["type"].([]any); ok {
		for _, raw := range types {
			t, ok := raw.(string)
			if !ok {
				return errors.New("invalid schema type union")
			}
			branch := maps.Clone(schema)
			branch["type"] = t
			if validateValue(branch, v) == nil {
				return nil
			}
		}
		return errors.New("value does not match schema type union")
	}
	t, _ := schema["type"].(string)
	valid := false
	switch t {
	case "null":
		valid = v == nil
	case "string":
		_, valid = v.(string)
	case "integer":
		switch v.(type) {
		case int, int64:
			valid = true
		}
	case "number":
		switch v.(type) {
		case int, int64, float64:
			valid = true
		}
	case "boolean":
		_, valid = v.(bool)
	case "array":
		if a, ok := v.([]string); ok {
			valid = true
			item, ok := schema["items"].(object)
			if !ok {
				return errors.New("array item schema missing")
			}
			for _, x := range a {
				if err := validateValue(item, x); err != nil {
					return err
				}
			}
		}
		if a, ok := v.([]object); ok {
			valid = true
			if item, ok := schema["items"].(object); ok {
				for _, x := range a {
					if err := validateValue(item, x); err != nil {
						return err
					}
				}
			}
		}
	case "object":
		if o, ok := v.(object); ok {
			valid = true
			p, _ := schema["properties"].(object)
			for k, val := range o {
				child, ok := p[k].(object)
				if !ok {
					return errors.New("unknown nested property")
				}
				if err := validateValue(child, val); err != nil {
					return err
				}
			}
			if required, ok := schema["required"].([]any); ok {
				for _, r := range required {
					k, _ := r.(string)
					if _, ok := o[k]; !ok {
						return errors.New("missing nested property")
					}
				}
			}
		}
	}
	if !valid {
		return errors.New("unsupported or mismatched input schema type")
	}
	if e, ok := schema["enum"].([]any); ok {
		for _, x := range e {
			if fmt.Sprint(x) == fmt.Sprint(v) {
				return nil
			}
		}
		return errors.New("value is outside schema enum")
	}
	return nil
}

func (c *Client) rpc(ctx context.Context, method string, params any, notification bool) (any, error) {
	c.id++
	body := object{"jsonrpc": "2.0", "method": method}
	if params != nil {
		body["params"] = params
	}
	if !notification {
		body["id"] = c.id
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	token, err := c.tokens.Access(ctx, c.http)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.endpoint, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("MCP-Protocol-Version", protocol)
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, errors.New("MCP transport failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("MCP HTTP %d (no automatic replay)", resp.StatusCode)
	}
	if method == "initialize" {
		c.session = resp.Header.Get("Mcp-Session-Id")
	}
	if notification {
		_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponse))
		return nil, err
	}
	var envelope object
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		scanner := bufio.NewScanner(io.LimitReader(resp.Body, maxResponse+1))
		scanner.Buffer(make([]byte, 4096), maxResponse)
		var event strings.Builder
		size := 0
		for scanner.Scan() {
			line := scanner.Text()
			size += len(line) + 1
			if size > maxResponse {
				return nil, errors.New("MCP response too large")
			}
			if data, ok := strings.CutPrefix(line, "data:"); ok {
				event.WriteString(strings.TrimPrefix(data, " "))
				event.WriteByte('\n')
			}
			if line == "" && event.Len() > 0 {
				v, e := decode([]byte(event.String()))
				event.Reset()
				if e != nil {
					return nil, e
				}
				if o, ok := v.(object); ok && o["id"] == json.Number(fmt.Sprint(c.id)) {
					envelope = o
					break
				}
			}
		}
		if err = scanner.Err(); err != nil {
			return nil, errors.New("MCP SSE read failed")
		}
	} else if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		b, err = io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
		if err != nil {
			return nil, err
		}
		if len(b) > maxResponse {
			return nil, errors.New("MCP response too large")
		}
		v, err := decode(b)
		if err != nil {
			return nil, err
		}
		envelope, _ = v.(object)
	} else {
		return nil, errors.New("unsupported MCP response content type")
	}
	if envelope == nil || envelope["jsonrpc"] != "2.0" || envelope["id"] != json.Number(fmt.Sprint(c.id)) {
		return nil, errors.New("MCP response identity mismatch")
	}
	if envelope["error"] != nil {
		return nil, errors.New("MCP JSON-RPC error")
	}
	result, ok := envelope["result"]
	if !ok {
		return nil, errors.New("MCP result missing")
	}
	return result, nil
}

func decode(b []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, errors.New("invalid provider JSON")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil, errors.New("provider JSON has trailing content")
	}
	return v, nil
}
