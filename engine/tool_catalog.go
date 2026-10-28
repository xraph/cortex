package engine

import (
	"fmt"

	"github.com/xraph/cortex/llm"
)

// ToolKind says where a tool in the catalog comes from.
type ToolKind string

const (
	// ToolBuiltin is a tool the engine implements itself.
	ToolBuiltin ToolKind = "builtin"
	// ToolRegistered is a host tool added with WithTool.
	ToolRegistered ToolKind = "registered"
	// ToolExternal is advertised to the model but run by the host, added
	// with WithExternalTool.
	ToolExternal ToolKind = "external"
)

// ToolInfo describes one tool the engine can offer a model.
type ToolInfo struct {
	Def       llm.Tool
	Kind      ToolKind
	RetrySafe bool
}

// Tools lists every tool in dispatch order: the builtins enabled for this
// external tools. A name is listed once, at the first definition dispatch
// reaches.
func (e *Engine) Tools() []ToolInfo {
	builtin := e.builtinTools()
	external := e.externalToolDefs()
	out := make([]ToolInfo, 0, len(builtin)+len(e.tools)+len(external))
	seen := make(map[string]bool, cap(out))

	for _, def := range builtin {
		seen[def.Name] = true
		out = append(out, ToolInfo{Def: def, Kind: ToolBuiltin, RetrySafe: e.retrySafe[def.Name]})
	}
	for _, rt := range e.tools {
		if seen[rt.def.Name] {
			continue
		}
		seen[rt.def.Name] = true
		out = append(out, ToolInfo{Def: rt.def, Kind: ToolRegistered, RetrySafe: e.retrySafe[rt.def.Name]})
	}
	for _, def := range external {
		if seen[def.Name] {
			continue
		}
		seen[def.Name] = true
		out = append(out, ToolInfo{Def: def, Kind: ToolExternal, RetrySafe: e.retrySafe[def.Name]})
	}

	return out
}

// ToolRetrySafe reports whether a tool was marked safe to run again.
func (e *Engine) ToolRetrySafe(name string) bool {
	return e.retrySafe[name]
}

// externalToolDefs is the one place the catalog reads external tool
// definitions, so the way they are stored can change without touching
// Tools.
func (e *Engine) externalToolDefs() []llm.Tool {
	return e.externalTools
}

// configureRetrySafe checks the WithToolRetrySafe names against the
// registered tools, once every option has run.
func (e *Engine) configureRetrySafe() error {
	if len(e.retrySafeNames) == 0 {
		return nil
	}
	registered := make(map[string]bool, len(e.tools))
	for _, rt := range e.tools {
		registered[rt.def.Name] = true
	}
	e.retrySafe = make(map[string]bool, len(e.retrySafeNames))
	for _, name := range e.retrySafeNames {
		if !registered[name] {
			return fmt.Errorf("cortex: retry-safe tool %q is not registered", name)
		}
		e.retrySafe[name] = true
	}

	return nil
}
