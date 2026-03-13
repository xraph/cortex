package dashboard

import (
	"context"

	"github.com/a-h/templ"

	"github.com/xraph/forge/extensions/dashboard/contributor"

	"github.com/xraph/cortex/id"
)

// PluginWidget describes a widget contributed by a cortex plugin.
type PluginWidget struct {
	ID         string
	Title      string
	Size       string // "sm", "md", "lg"
	RefreshSec int
	Render     func(ctx context.Context) templ.Component
}

// PluginPage describes an extra page route contributed by a plugin.
type PluginPage struct {
	Route  string
	Label  string
	Icon   string
	Render func(ctx context.Context) templ.Component
}

// Plugin is optionally implemented by cortex plugins
// to contribute UI sections to the cortex dashboard contributor.
type Plugin interface {
	DashboardWidgets(ctx context.Context) []PluginWidget
	DashboardSettingsPanel(ctx context.Context) templ.Component
	DashboardPages() []PluginPage
}

// AgentDetailContributor is optionally implemented by plugins that want to
// contribute a section to the agent detail page.
type AgentDetailContributor interface {
	DashboardAgentDetailSection(ctx context.Context, agentID id.AgentID) templ.Component
}

// PersonaDetailContributor is optionally implemented by plugins that want to
// contribute a section to the persona detail page.
type PersonaDetailContributor interface {
	DashboardPersonaDetailSection(ctx context.Context, personaID id.PersonaID) templ.Component
}

// RunDetailContributor is optionally implemented by plugins that want to
// contribute a section to the run detail page.
type RunDetailContributor interface {
	DashboardRunDetailSection(ctx context.Context, runID id.AgentRunID) templ.Component
}

// PageContributor is an enhanced interface for plugins that need
// access to route parameters when rendering dashboard pages.
type PageContributor interface {
	DashboardNavItems() []contributor.NavItem
	DashboardRenderPage(ctx context.Context, route string, params contributor.Params) (templ.Component, error)
}

// ChatContributor allows plugins to extend the chat page with toolbar
// items and per-message action buttons.
type ChatContributor interface {
	// DashboardChatToolbar returns a component rendered in the chat page toolbar.
	DashboardChatToolbar(ctx context.Context) templ.Component
	// DashboardChatMessageActions returns a component rendered alongside each
	// message of the given role (e.g. copy, bookmark, replay).
	DashboardChatMessageActions(ctx context.Context, role string) templ.Component
}

// PlaygroundContributor allows plugins to extend the playground page with
// additional sidebar panels and tabs.
type PlaygroundContributor interface {
	// DashboardPlaygroundPanel returns a component rendered in the playground sidebar.
	DashboardPlaygroundPanel(ctx context.Context) templ.Component
	// DashboardPlaygroundTab returns a tab label and content component for
	// an additional playground tab.
	DashboardPlaygroundTab(ctx context.Context) (label string, content templ.Component)
}
