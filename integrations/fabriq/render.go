package fabriqbrain

import "github.com/xraph/fabriq/core/agent"

// defaultRender renders a recalled item as its row JSON verbatim. Hosts that
// want summary/altitude text can override via WithRenderer.
func defaultRender(it agent.ContextItem) string {
	return string(it.Row)
}
