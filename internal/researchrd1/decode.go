package researchrd1

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
)

const MaxPacketBytes = 256 << 20

// DecodePacket rejects duplicate JSON keys and floating/exponent money before
// typed decoding. encoding/json alone otherwise accepts duplicate keys and null
// integers, which can hide an omitted fee behind a later explicit zero.
func DecodePacket(raw []byte) (Packet, error) {
	return decodePacket[Packet](raw)
}

func decodePacket[T any](raw []byte) (T, error) {
	var p T
	if len(raw) == 0 || len(raw) > MaxPacketBytes {
		return p, errors.New("RD1 packet size outside bounded envelope")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	tokens := 0
	var value func(int) error
	value = func(depth int) error {
		if depth > 32 || tokens > 10_000_000 {
			return errors.New("RD1 excessive JSON nesting/token count")
		}
		token, err := d.Token()
		if err != nil {
			return err
		}
		tokens++
		switch t := token.(type) {
		case json.Delim:
			switch t {
			case '{':
				keys := map[string]bool{}
				for d.More() {
					key, err := d.Token()
					if err != nil {
						return err
					}
					name, ok := key.(string)
					if !ok || keys[name] || len(keys) >= 100000 {
						return errors.New("RD1 duplicate/invalid/excessive JSON keys")
					}
					keys[name] = true
					if err := value(depth + 1); err != nil {
						return err
					}
				}
				end, err := d.Token()
				if err != nil || end != json.Delim('}') {
					return errors.New("RD1 unterminated JSON object")
				}
			case '[':
				for d.More() {
					if err := value(depth + 1); err != nil {
						return err
					}
				}
				end, err := d.Token()
				if err != nil || end != json.Delim(']') {
					return errors.New("RD1 unterminated JSON array")
				}
			default:
				return errors.New("RD1 unexpected JSON delimiter")
			}
		case json.Number:
			if _, err := strconv.ParseInt(string(t), 10, 64); err != nil {
				return errors.New("RD1 packet numeric fields require exact int64 units")
			}
		}
		return nil
	}
	if err := value(0); err != nil {
		return p, err
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return p, errors.New("RD1 multiple JSON packet values")
	}
	typed := json.NewDecoder(bytes.NewReader(raw))
	typed.DisallowUnknownFields()
	if err := typed.Decode(&p); err != nil {
		var zero T
		return zero, fmt.Errorf("RD1 packet schema: %w", err)
	}
	return p, nil
}
