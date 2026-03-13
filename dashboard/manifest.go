package dashboard

import (
	"context"

	"github.com/xraph/forge/extensions/dashboard/contributor"

	"github.com/xraph/cortex/dashboard/components"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/plugin"
)

// NewManifest builds a contributor.Manifest for the cortex dashboard.
// When modelSource is non-nil, a "Models" nav item is added under the "Gateway" group.
// When safetySource is non-nil, "Safety Profiles" and "Safety Scans" nav items are added.
func NewManifest(_ *engine.Engine, plugins []plugin.Extension, modelSource ModelSource, safetySource SafetySource, knowledgeSource KnowledgeSource) *contributor.Manifest {
	m := &contributor.Manifest{
		Name:        "cortex",
		DisplayName: "Cortex",
		Icon:        "bot",
		Version:     "0.1.0",
		Layout:      "extension",
		ShowSidebar: boolPtr(true),
		TopbarConfig: &contributor.TopbarConfig{
			Title:       "Cortex",
			LogoIcon:    "bot",
			AccentColor: "#8b5cf6",
			ShowSearch:  true,
			Actions: []contributor.TopbarAction{
				{Label: "API Docs", Icon: "file-text", Href: "/docs", Variant: "ghost"},
			},
		},
		SidebarFooterContent: components.FooterAPIDocsLink("/docs"),
		Nav:                  baseNav(),
		Widgets:              baseWidgets(),
		Settings:             baseSettings(),
		Capabilities: []string{
			"searchable",
		},
	}

	// Conditionally add Models page when a model source (e.g. nexus) is available.
	if modelSource != nil {
		m.Nav = append(m.Nav, contributor.NavItem{
			Label: "Models", Path: "/models", Icon: "sparkles", Group: "Gateway", Priority: 10,
		})
	}

	// Conditionally add Knowledge page when a knowledge source (e.g. weave) is available.
	if knowledgeSource != nil {
		m.Nav = append(m.Nav, contributor.NavItem{
			Label: "Knowledge", Path: "/knowledge", Icon: "book-open", Group: "Knowledge", Priority: 11,
		})
		m.Widgets = append(m.Widgets, contributor.WidgetDescriptor{
			ID:          "cortex-knowledge-stats",
			Title:       "Knowledge Overview",
			Description: "Weave knowledge base statistics",
			Size:        "md",
			RefreshSec:  30,
			Group:       "Knowledge",
		})
	}

	// Conditionally add Safety pages when a safety source (e.g. shield) is available.
	if safetySource != nil {
		m.Nav = append(m.Nav,
			contributor.NavItem{Label: "Safety Profiles", Path: "/safety/profiles", Icon: "shield-check", Group: "Safety", Priority: 10},
			contributor.NavItem{Label: "Safety Scans", Path: "/safety/scans", Icon: "scan-line", Group: "Safety", Priority: 11},
		)
		m.Widgets = append(m.Widgets, contributor.WidgetDescriptor{
			ID:          "cortex-safety-stats",
			Title:       "Safety Overview",
			Description: "Shield scan statistics for agent runs",
			Size:        "md",
			RefreshSec:  30,
			Group:       "Safety",
		})
	}

	// Merge plugin-contributed nav items and widgets.
	for _, p := range plugins {
		if dpc, ok := p.(PageContributor); ok {
			m.Nav = append(m.Nav, dpc.DashboardNavItems()...)
		}

		dp, ok := p.(Plugin)
		if !ok {
			continue
		}

		for _, pp := range dp.DashboardPages() {
			m.Nav = append(m.Nav, contributor.NavItem{
				Label:    pp.Label,
				Path:     pp.Route,
				Icon:     pp.Icon,
				Group:    "Cortex",
				Priority: 10,
			})
		}

		for _, pw := range dp.DashboardWidgets(context.Background()) {
			m.Widgets = append(m.Widgets, contributor.WidgetDescriptor{
				ID:         pw.ID,
				Title:      pw.Title,
				Size:       pw.Size,
				RefreshSec: pw.RefreshSec,
				Group:      "Cortex",
			})
		}
	}

	return m
}

func baseNav() []contributor.NavItem {
	return []contributor.NavItem{
		{Label: "Overview", Path: "/", Icon: "layout-dashboard", Group: "Cortex", Priority: 0},
		{Label: "Chat", Path: "/chat", Icon: "message-square", Group: "Cortex", Priority: 0},
		{Label: "Playground", Path: "/playground", Icon: "flask-conical", Group: "Cortex", Priority: 0},
		{Label: "Agents", Path: "/agents", Icon: "bot", Group: "Design", Priority: 1},
		{Label: "Personas", Path: "/personas", Icon: "user-circle", Group: "Design", Priority: 2},
		{Label: "Tools", Path: "/tools", Icon: "terminal", Group: "Composition", Priority: 3},
		{Label: "Skills", Path: "/skills", Icon: "wrench", Group: "Composition", Priority: 4},
		{Label: "Traits", Path: "/traits", Icon: "brain", Group: "Composition", Priority: 5},
		{Label: "Behaviors", Path: "/behaviors", Icon: "zap", Group: "Composition", Priority: 6},
		{Label: "Runs", Path: "/runs", Icon: "play", Group: "Operations", Priority: 7},
		{Label: "Checkpoints", Path: "/checkpoints", Icon: "shield-check", Group: "Operations", Priority: 8},
		{Label: "Memory", Path: "/memory", Icon: "database", Group: "Operations", Priority: 9},
	}
}

func baseWidgets() []contributor.WidgetDescriptor {
	return []contributor.WidgetDescriptor{
		{
			ID:          "cortex-stats",
			Title:       "Entity Stats",
			Description: "Cortex entity counts",
			Size:        "md",
			RefreshSec:  60,
			Group:       "Cortex",
		},
		{
			ID:          "cortex-recent-runs",
			Title:       "Recent Runs",
			Description: "Recent agent run results",
			Size:        "lg",
			RefreshSec:  15,
			Group:       "Cortex",
		},
		{
			ID:          "cortex-active-checkpoints",
			Title:       "Active Checkpoints",
			Description: "Pending human-in-the-loop checkpoints",
			Size:        "md",
			RefreshSec:  10,
			Group:       "Cortex",
		},
	}
}

func baseSettings() []contributor.SettingsDescriptor {
	return []contributor.SettingsDescriptor{
		{
			ID:          "cortex-config",
			Title:       "Engine Settings",
			Description: "Configure Cortex engine behavior",
			Group:       "Cortex",
			Icon:        "bot",
		},
	}
}

func boolPtr(b bool) *bool { return &b }
