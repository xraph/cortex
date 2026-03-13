package sentinel

import (
	"context"

	"github.com/a-h/templ"

	"github.com/xraph/cortex/dashboard"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/sentinel/pages"

	"github.com/xraph/sentinel/evalrun"
	"github.com/xraph/sentinel/suite"
)

// Compile-time interface checks for dashboard contributions.
var (
	_ dashboard.Plugin                 = (*Plugin)(nil)
	_ dashboard.AgentDetailContributor = (*Plugin)(nil)
	_ dashboard.RunDetailContributor   = (*Plugin)(nil)
	_ dashboard.PlaygroundContributor  = (*Plugin)(nil)
)

// ──────────────────────────────────────────────────
// DashboardPlugin
// ──────────────────────────────────────────────────

func (p *Plugin) DashboardWidgets(_ context.Context) []dashboard.PluginWidget {
	return []dashboard.PluginWidget{
		{
			ID:         "sentinel-eval-stats",
			Title:      "Evaluation Stats",
			Size:       "md",
			RefreshSec: 60,
			Render: func(ctx context.Context) templ.Component {
				data := p.fetchWidgetData(ctx)
				return pages.EvalStatsWidget(data)
			},
		},
	}
}

func (p *Plugin) DashboardSettingsPanel(_ context.Context) templ.Component {
	return nil
}

func (p *Plugin) DashboardPages() []dashboard.PluginPage {
	return []dashboard.PluginPage{
		{
			Route: "evaluations",
			Label: "Evaluations",
			Icon:  "flask-conical",
			Render: func(_ context.Context) templ.Component {
				// Renders a placeholder that links to Sentinel's own dashboard.
				return nil
			},
		},
	}
}

// ──────────────────────────────────────────────────
// AgentDetailContributor
// ──────────────────────────────────────────────────

func (p *Plugin) DashboardAgentDetailSection(ctx context.Context, agentID id.AgentID) templ.Component {
	data := p.fetchAgentEvalSummary(ctx, agentID.String())
	return pages.AgentEvalSummary(data)
}

// ──────────────────────────────────────────────────
// RunDetailContributor
// ──────────────────────────────────────────────────

func (p *Plugin) DashboardRunDetailSection(ctx context.Context, runID id.AgentRunID) templ.Component {
	data := p.fetchRunEvalData(ctx, runID.String())
	return pages.RunEvalSection(data)
}

// ──────────────────────────────────────────────────
// PlaygroundContributor
// ──────────────────────────────────────────────────

func (p *Plugin) DashboardPlaygroundPanel(ctx context.Context) templ.Component {
	data := p.fetchPlaygroundPanelData(ctx)
	return pages.PlaygroundEvalPanel(data)
}

func (p *Plugin) DashboardPlaygroundTab(_ context.Context) (string, templ.Component) {
	data := &pages.PlaygroundEvalResultData{HasResults: false}
	return "Eval Results", pages.PlaygroundEvalTab(data)
}

// ──────────────────────────────────────────────────
// Data fetchers (internal)
// ──────────────────────────────────────────────────

func (p *Plugin) fetchWidgetData(ctx context.Context) *pages.EvalWidgetData {
	data := &pages.EvalWidgetData{}

	suites, err := p.eng.ListSuites(ctx, &suite.ListFilter{})
	if err == nil {
		data.TotalSuites = len(suites)
	}

	runs, err := p.eng.ListRuns(ctx, &evalrun.ListFilter{Limit: 100})
	if err == nil {
		data.TotalEvals = len(runs)
		var totalPassRate float64
		for _, r := range runs {
			totalPassRate += r.PassRate
			if r.State == evalrun.StateCompleted && r.PassRate < 0.8 {
				data.RecentFails++
			}
		}
		if len(runs) > 0 {
			data.AvgPassRate = totalPassRate / float64(len(runs))
		}
	}

	return data
}

func (p *Plugin) fetchAgentEvalSummary(_ context.Context, _ string) *pages.EvalSummaryData {
	// TODO: query suites tagged for this agent and fetch latest run stats.
	return &pages.EvalSummaryData{HasEvals: false}
}

func (p *Plugin) fetchRunEvalData(_ context.Context, _ string) *pages.RunEvalData {
	// TODO: query eval results linked to this Cortex run ID.
	return &pages.RunEvalData{HasEval: false}
}

func (p *Plugin) fetchPlaygroundPanelData(ctx context.Context) *pages.PlaygroundEvalData {
	data := &pages.PlaygroundEvalData{}

	suites, err := p.eng.ListSuites(ctx, &suite.ListFilter{})
	if err == nil {
		for _, s := range suites {
			data.Suites = append(data.Suites, pages.PlaygroundSuiteOption{
				ID:   s.ID.String(),
				Name: s.Name,
			})
		}
	}

	return data
}
