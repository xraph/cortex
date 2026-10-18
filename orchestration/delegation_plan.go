package orchestration

import (
	"encoding/json"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/xraph/cortex"
)

const (
	maxDelegationPlanBytes = 256 * 1024
	maxDelegationTaskBytes = 64 * 1024
	maxDelegationWorkers   = 64
)

// InvalidDelegationPlanError identifies a rejected plan without exposing model text.
type InvalidDelegationPlanError struct{ Reason string }

func (e *InvalidDelegationPlanError) Error() string {
	return cortex.ErrInvalidDelegationPlan.Error() + ": " + e.Reason
}

// Unwrap supports errors.Is with cortex.ErrInvalidDelegationPlan.
func (e *InvalidDelegationPlanError) Unwrap() error { return cortex.ErrInvalidDelegationPlan }

func invalidPlan(reason string) error { return &InvalidDelegationPlanError{Reason: reason} }

// parseDelegations requires exactly one assignment for each required worker.
// Token decoding rejects duplicate fields that ordinary JSON unmarshaling loses.
func parseDelegations(output string, workers []Participant) ([]delegation, error) {
	if len(output) > maxDelegationPlanBytes || !utf8.ValidString(output) {
		return nil, invalidPlan("invalid plan size or encoding")
	}
	if len(workers) == 0 || len(workers) > maxDelegationWorkers {
		return nil, invalidPlan("invalid worker count")
	}
	pending := make(map[string]bool, len(workers))
	for _, w := range workers {
		if strings.TrimSpace(w.AgentName) == "" || pending[w.AgentName] {
			return nil, invalidPlan("invalid worker roster")
		}
		pending[w.AgentName] = true
	}
	dec := json.NewDecoder(strings.NewReader(output))
	if !consumeDelimiter(dec, '[') {
		return nil, invalidPlan("expected a JSON array")
	}
	tasks := make([]delegation, 0, len(workers))
	for dec.More() {
		if len(tasks) >= len(workers) {
			return nil, invalidPlan("too many assignments")
		}
		d, err := decodeDelegation(dec)
		if err != nil {
			return nil, err
		}
		if !pending[d.Agent] {
			return nil, invalidPlan("unknown or duplicate worker")
		}
		delete(pending, d.Agent)
		tasks = append(tasks, d)
	}
	if !consumeDelimiter(dec, ']') {
		return nil, invalidPlan("unterminated plan")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, invalidPlan("trailing plan content")
	}
	if len(pending) != 0 {
		return nil, invalidPlan("missing required assignment")
	}
	return tasks, nil
}

func consumeDelimiter(dec *json.Decoder, want json.Delim) bool {
	token, err := dec.Token()
	return err == nil && token == want
}

func decodeDelegation(dec *json.Decoder) (delegation, error) {
	var d delegation
	if !consumeDelimiter(dec, '{') {
		return d, invalidPlan("expected an assignment object")
	}
	fields := make(map[string]bool, 2)
	for dec.More() {
		token, err := dec.Token()
		key, ok := token.(string)
		if err != nil || !ok || (key != "agent" && key != "task") || fields[key] {
			return d, invalidPlan("unknown or duplicate assignment field")
		}
		fields[key] = true
		token, err = dec.Token()
		value, ok := token.(string)
		if err != nil || !ok || strings.TrimSpace(value) == "" {
			return d, invalidPlan("assignment fields must be nonempty strings")
		}
		if key == "agent" {
			d.Agent = value
		} else {
			d.Task = value
		}
	}
	if !consumeDelimiter(dec, '}') || len(fields) != 2 {
		return d, invalidPlan("incomplete assignment")
	}
	if len(d.Task) > maxDelegationTaskBytes {
		return d, invalidPlan("task exceeds size limit")
	}
	return d, nil
}
