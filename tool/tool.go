package tool

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/llm"
)

type options struct {
	retrySafe bool
}

// Option adjusts how New and Register build a tool.
type Option func(*options)

// RetrySafe declares that the handler may be run again after a crash: it
// only reads, or it is idempotent through the effect journal. Without it
// the engine treats an interrupted call as uncertain.
func RetrySafe() Option {
	return func(o *options) { o.retrySafe = true }
}

// New builds a tool definition and handler from a typed function. The
// schema comes from In, which must be a struct. It panics when the schema
// cannot be built, because that is a mistake in the program, not in a
// call.
//
// The handler validates the call's arguments against the schema before fn
// runs. A failure comes back as an ordinary tool error the model can read.
// An Out of string or json.RawMessage is returned as is. Anything else is
// marshalled to JSON. fn's own error is returned unwrapped, so
// cortex.ErrEffectUncertain still matches with errors.Is.
func New[In, Out any](name, description string, fn func(ctx context.Context, inv cortex.Invocation, in In) (Out, error), opts ...Option) (llm.Tool, engine.ToolHandler) {
	def, handler, _ := build(name, description, fn, opts)
	return def, handler
}

// Register is New plus the engine wiring: it registers the tool with
// engine.WithTool and, when RetrySafe is given, marks it with
// engine.WithToolRetrySafe.
func Register[In, Out any](name, description string, fn func(ctx context.Context, inv cortex.Invocation, in In) (Out, error), opts ...Option) engine.Option {
	def, handler, o := build(name, description, fn, opts)
	withTool := engine.WithTool(def, handler)
	if !o.retrySafe {
		return withTool
	}
	withSafe := engine.WithToolRetrySafe(name)
	return func(e *engine.Engine) error {
		if err := withTool(e); err != nil {
			return err
		}
		return withSafe(e)
	}
}

func build[In, Out any](name, description string, fn func(ctx context.Context, inv cortex.Invocation, in In) (Out, error), opts []Option) (llm.Tool, engine.ToolHandler, options) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	root, err := rootSchema[In]()
	if err != nil {
		panic(fmt.Sprintf("tool %q: %v", name, err))
	}
	params, err := schemaMap(root)
	if err != nil {
		panic(fmt.Sprintf("tool %q: %v", name, err))
	}
	resolved, err := root.Resolve(nil)
	if err != nil {
		panic(fmt.Sprintf("tool %q: resolve schema: %v", name, err))
	}

	def := llm.Tool{Name: name, Description: description, Parameters: params}
	handler := func(ctx context.Context, inv cortex.Invocation) (string, error) {
		args := inv.Call.Arguments
		if args == "" {
			args = "{}"
		}
		// Decoded as plain float64: jsonschema-go reads a json.Number as a
		// string, so UseNumber here would fail every numeric property.
		var generic map[string]any
		if err := json.Unmarshal([]byte(args), &generic); err != nil {
			return "", fmt.Errorf("tool %q: invalid arguments: %w", name, err)
		}
		if err := resolved.Validate(generic); err != nil {
			return "", fmt.Errorf("tool %q: invalid arguments: %w", name, err)
		}
		var in In
		if err := json.Unmarshal([]byte(args), &in); err != nil {
			return "", fmt.Errorf("tool %q: invalid arguments: %w", name, err)
		}

		out, err := fn(ctx, inv, in)
		if err != nil {
			return "", err
		}
		return encode(name, out)
	}
	return def, handler, o
}

// encode renders a handler result for the model.
func encode(name string, out any) (string, error) {
	switch v := out.(type) {
	case string:
		return v, nil
	case json.RawMessage:
		return string(v), nil
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("tool %q: encode result: %w", name, err)
	}
	return string(raw), nil
}
