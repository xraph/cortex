// Package canonical encodes versioned optimization content and rejects ambiguous JSON.
package canonical

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Marshal uses Go's deterministic map ordering while preserving array order and
// exact json.Number values. Callers define the schema and omit display metadata.
func Marshal(value any) ([]byte, error) { return json.Marshal(value) }

// Decode accepts one JSON object with no unknown or duplicate members.
func Decode(data []byte, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token != json.Delim('{') {
		return fmt.Errorf("canonical: expected object")
	}
	if objectErr := object(decoder, 0); objectErr != nil {
		return objectErr
	}
	if _, err = decoder.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return fmt.Errorf("canonical: trailing JSON")
	}
	decoder = json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	return decoder.Decode(into)
}

func object(decoder *json.Decoder, depth int) error {
	seen := make(map[string]bool)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok {
			return fmt.Errorf("canonical: expected member name")
		}
		if seen[key] {
			return fmt.Errorf("canonical: duplicate member %q", key)
		}
		seen[key] = true
		if valueErr := value(decoder, depth+1); valueErr != nil {
			return valueErr
		}
	}
	_, err := decoder.Token()
	return err
}

func value(decoder *json.Decoder, depth int) error {
	if depth > 64 {
		return fmt.Errorf("canonical: maximum nesting exceeded")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	switch token {
	case json.Delim('{'):
		return object(decoder, depth)
	case json.Delim('['):
		for decoder.More() {
			if valueErr := value(decoder, depth+1); valueErr != nil {
				return valueErr
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return nil
	}
}
