// Package tool builds engine tools from typed Go handlers. The argument
// schema is generated from the input struct, and every call is validated
// against it before the handler runs.
package tool

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
)

// Schema returns the JSON Schema for In as the map an llm.Tool carries in
// Parameters. In must be a struct. The root is a closed object: unknown
// properties are rejected. Numbers decode as json.Number, the same shape
// the engine's hand-written tool schemas use, so a prepared snapshot
// hashes both alike.
func Schema[In any]() (map[string]any, error) {
	s, err := rootSchema[In]()
	if err != nil {
		return nil, err
	}
	return schemaMap(s)
}

// rootSchema generates the closed root object schema for In.
func rootSchema[In any]() (*jsonschema.Schema, error) {
	s, err := jsonschema.For[In](nil)
	if err != nil {
		return nil, fmt.Errorf("generate schema: %w", err)
	}
	if s.Type != "object" {
		return nil, errors.New("input must be a struct")
	}
	s.Schema = ""
	s.ID = ""
	s.AdditionalProperties = &jsonschema.Schema{Not: &jsonschema.Schema{}}
	return s, nil
}

// schemaMap round-trips s through JSON into a generic map.
func schemaMap(s *jsonschema.Schema) (map[string]any, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("encode schema: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("decode schema: %w", err)
	}
	return out, nil
}
