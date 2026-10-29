// Package mcpbridge registers the tools of an MCP server as cortex tools.
//
// The host owns the *mcp.ClientSession and its lifetime. The bridge lists the
// server's tools, maps each to an llm.Tool plus a handler that calls the server,
// and tells the engine which of them are safe to run again after a crash.
package mcpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/llm"
)

// defaultMaxResultBytes caps a tool result when Options.MaxResultBytes is unset.
const defaultMaxResultBytes = 1 << 20

var nameRE = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// Options tunes how a server's tools are exposed.
type Options struct {
	// Prefix is prepended to every tool name, for example "github_".
	Prefix string
	// Include filters the server's tools. Nil includes every tool.
	Include func(t *mcp.Tool) bool
	// MaxResultBytes caps a tool result. A larger result is a tool error.
	// Zero means 1 MiB.
	MaxResultBytes int
}

// Tool is one server tool, ready to register with the engine.
type Tool struct {
	// Def is the definition advertised to the model. Name carries the prefix
	// and Parameters is the server's input schema.
	Def llm.Tool
	// Handler calls the server.
	Handler engine.ToolHandler
	// ReadOnly mirrors the readOnlyHint annotation.
	ReadOnly bool
	// Destructive mirrors the destructiveHint annotation. It defaults to
	// true when the annotation is unset and the tool is not read-only.
	Destructive bool
}

// Tools lists the server's tools, following pagination, and maps each one.
// It returns an error naming every included tool whose prefixed name does not
// match ^[a-zA-Z0-9_-]{1,64}$.
func Tools(ctx context.Context, cs *mcp.ClientSession, opts Options) ([]Tool, error) {
	limit := opts.MaxResultBytes
	if limit <= 0 {
		limit = defaultMaxResultBytes
	}

	var (
		out     []Tool
		invalid []string
	)
	for t, err := range cs.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("mcpbridge: list tools: %w", err)
		}
		if opts.Include != nil && !opts.Include(t) {
			continue
		}
		name := opts.Prefix + t.Name
		if !nameRE.MatchString(name) {
			invalid = append(invalid, fmt.Sprintf("%q", name))
			continue
		}
		readOnly := t.Annotations != nil && t.Annotations.ReadOnlyHint
		destructive := false
		if !readOnly {
			destructive = t.Annotations == nil || t.Annotations.DestructiveHint == nil || *t.Annotations.DestructiveHint
		}
		out = append(out, Tool{
			Def: llm.Tool{
				Name:        name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
			Handler:     handler(cs, t.Name, readOnly, limit),
			ReadOnly:    readOnly,
			Destructive: destructive,
		})
	}
	if len(invalid) > 0 {
		return nil, fmt.Errorf("mcpbridge: tool names must match %s: %s", nameRE, strings.Join(invalid, ", "))
	}
	return out, nil
}

// EngineOptions returns a WithTool option for every tool, plus a
// WithToolRetrySafe option naming the read-only ones.
func EngineOptions(ctx context.Context, cs *mcp.ClientSession, opts Options) ([]engine.Option, error) {
	tools, err := Tools(ctx, cs, opts)
	if err != nil {
		return nil, err
	}
	out := make([]engine.Option, 0, len(tools)+1)
	var safe []string
	for _, t := range tools {
		out = append(out, engine.WithTool(t.Def, t.Handler))
		if t.ReadOnly {
			safe = append(safe, t.Def.Name)
		}
	}
	if len(safe) > 0 {
		out = append(out, engine.WithToolRetrySafe(safe...))
	}
	return out, nil
}

func handler(cs *mcp.ClientSession, remoteName string, readOnly bool, limit int) engine.ToolHandler {
	return func(ctx context.Context, inv cortex.Invocation) (string, error) {
		args := json.RawMessage(inv.Call.Arguments)
		if strings.TrimSpace(inv.Call.Arguments) == "" {
			args = json.RawMessage(`{}`)
		}
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: remoteName, Arguments: args})
		if err != nil {
			// The request may have reached the server, so a tool that can
			// write must not be sent again on recovery.
			if readOnly {
				return "", fmt.Errorf("mcpbridge: call %s: %w", remoteName, err)
			}
			return "", fmt.Errorf("mcpbridge: call %s: %w: %w", remoteName, cortex.ErrEffectUncertain, err)
		}
		text, err := render(res)
		if err != nil {
			return "", fmt.Errorf("mcpbridge: %s result: %w", remoteName, err)
		}
		if res.IsError {
			return "", errors.New(text)
		}
		if len(text) > limit {
			return "", fmt.Errorf("mcpbridge: %s result is %d bytes, over the %d byte limit", remoteName, len(text), limit)
		}
		return text, nil
	}
}

func render(res *mcp.CallToolResult) (string, error) {
	if res.StructuredContent != nil && !res.IsError {
		b, err := json.Marshal(res.StructuredContent)
		if err != nil {
			return "", fmt.Errorf("encode structured content: %w", err)
		}
		return string(b), nil
	}
	parts := make([]string, 0, len(res.Content))
	for _, c := range res.Content {
		switch v := c.(type) {
		case *mcp.TextContent:
			parts = append(parts, v.Text)
		case *mcp.ImageContent:
			parts = append(parts, "[image content omitted]")
		case *mcp.AudioContent:
			parts = append(parts, "[audio content omitted]")
		case *mcp.ResourceLink:
			parts = append(parts, "[resource_link content omitted]")
		case *mcp.EmbeddedResource:
			parts = append(parts, "[resource content omitted]")
		default:
			parts = append(parts, "[unknown content omitted]")
		}
	}
	return strings.Join(parts, "\n"), nil
}
