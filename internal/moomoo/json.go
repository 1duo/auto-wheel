package moomoo

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"
)

func readBounded(r io.Reader, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, errors.New("JSON document is truncated or exceeds limit")
	}
	return raw, nil
}

func boundedJSON(r io.Reader, limit int64, out any) error {
	raw, err := readBounded(r, limit)
	if err != nil {
		return err
	}
	return exactJSON(raw, out)
}

func privateJSON(r io.Reader, limit int64, out any) error {
	raw, err := readBounded(r, limit)
	if err != nil {
		return err
	}
	return decodeJSON(raw, out, true)
}

func exactJSON(raw []byte, out any) error { return decodeJSON(raw, out, false) }

func decodeJSON(raw []byte, out any, strict bool) error {
	if !utf8.Valid(raw) {
		return errors.New("JSON is not valid UTF-8")
	}
	check := json.NewDecoder(bytes.NewReader(raw))
	check.UseNumber()
	if err := uniqueJSONValue(check, 0); err != nil {
		return err
	}
	if _, err := check.Token(); !errors.Is(err, io.EOF) {
		return errors.New("JSON has trailing content")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if strict {
		d.DisallowUnknownFields()
	}
	return d.Decode(out)
}

// Duplicate fields can turn an error into success or replace an account,
// token scope, amount or RPC identity. Reject them instead of choosing a value.
func uniqueJSONValue(d *json.Decoder, depth int) error {
	if depth > 128 {
		return errors.New("JSON nesting exceeds limit")
	}
	token, err := d.Token()
	if err != nil {
		return errors.New("invalid JSON value")
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			name, ok := key.(string)
			if err != nil || !ok || seen[name] {
				return errors.New("JSON object has invalid or duplicate fields")
			}
			seen[name] = true
			if err := uniqueJSONValue(d, depth+1); err != nil {
				return err
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("invalid JSON object")
		}
	case '[':
		for d.More() {
			if err := uniqueJSONValue(d, depth+1); err != nil {
				return err
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("invalid JSON array")
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	return nil
}
