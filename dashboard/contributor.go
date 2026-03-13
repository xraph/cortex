package dashboard

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/a-h/templ"

	"github.com/xraph/forge/extensions/dashboard/contributor"

	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/dashboard/components"
	"github.com/xraph/cortex/dashboard/pages"
	"github.com/xraph/cortex/dashboard/settings"
	"github.com/xraph/cortex/dashboard/widgets"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/memory"
	"github.com/xraph/cortex/plugin"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/trait"
)

var _ contributor.LocalContributor = (*Contributor)(nil)

// Contributor implements the dashboard LocalContributor interface for the
// cortex extension.
type Contributor struct {
	manifest        *contributor.Manifest
	engine          *engine.Engine
	plugins         []plugin.Extension
	modelSource     ModelSource
	safetySource    SafetySource
	knowledgeSource KnowledgeSource
}

// ContributorOption configures optional Contributor behaviour.
type ContributorOption func(*Contributor)

// WithModelSource sets an optional ModelSource (e.g. nexus) for model discovery.
func WithModelSource(ms ModelSource) ContributorOption {
	return func(c *Contributor) { c.modelSource = ms }
}

// WithSafetySource sets an optional SafetySource (e.g. shield) for safety visibility.
func WithSafetySource(ss SafetySource) ContributorOption {
	return func(c *Contributor) { c.safetySource = ss }
}

// WithKnowledgeSource sets an optional KnowledgeSource (e.g. weave) for knowledge visibility.
func WithKnowledgeSource(ks KnowledgeSource) ContributorOption {
	return func(c *Contributor) { c.knowledgeSource = ks }
}

// New creates a new cortex dashboard contributor.
func New(manifest *contributor.Manifest, eng *engine.Engine, plugins []plugin.Extension, opts ...ContributorOption) *Contributor {
	c := &Contributor{
		manifest: manifest,
		engine:   eng,
		plugins:  plugins,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Manifest returns the contributor manifest.
func (c *Contributor) Manifest() *contributor.Manifest { return c.manifest }

// RenderPage renders a page for the given route.
func (c *Contributor) RenderPage(ctx context.Context, route string, params contributor.Params) (templ.Component, error) {
	if c.engine == nil {
		return components.EmptyState("alert-circle", "Engine not initialized", "The Cortex engine is not available. Please check extension configuration."), nil
	}
	s := c.engine.Store()
	if s == nil {
		return components.EmptyState("database", "No store configured", "The Cortex dashboard requires a database store. Please configure a Grove driver or provide a store via engine options."), nil
	}
	comp, err := c.renderPageRoute(ctx, route, s, params)
	if err != nil {
		return nil, err
	}
	// Wrap every page in the PathRewriter so bare hx-get paths (e.g. "/personas/create")
	// are rewritten to the fully-qualified dashboard extension path at runtime.
	pagesBase := params.BasePath + "/ext/" + c.manifest.Name + "/pages"
	return templ.ComponentFunc(func(tCtx context.Context, w io.Writer) error {
		return components.PathRewriter(pagesBase).Render(templ.WithChildren(tCtx, comp), w)
	}), nil
}

func (c *Contributor) renderPageRoute(ctx context.Context, pageRoute string, s store.Store, params contributor.Params) (templ.Component, error) {
	// Normalize route: trim trailing slashes (except root), collapse doubles.
	pageRoute = strings.TrimRight(pageRoute, "/")
	if pageRoute == "" {
		pageRoute = "/"
	}

	// Check plugin-contributed pages first.
	for _, p := range c.plugins {
		if dpc, ok := p.(PageContributor); ok {
			if comp, err := dpc.DashboardRenderPage(ctx, pageRoute, params); err == nil && comp != nil {
				return comp, nil
			}
		}
	}

	for _, dp := range c.dashboardPlugins() {
		for _, pp := range dp.DashboardPages() {
			if pp.Route == pageRoute {
				return pp.Render(ctx), nil
			}
		}
	}

	switch pageRoute {
	case "/":
		return c.renderOverview(ctx, s)
	case "/agents":
		return c.renderAgents(ctx, s, params)
	case "/agents/detail":
		return c.renderAgentDetail(ctx, s, params)
	case "/agents/create":
		return c.renderAgentForm(ctx, s, params)
	case "/agents/edit":
		return c.renderAgentForm(ctx, s, params)
	case "/personas":
		return c.renderPersonas(ctx, s, params)
	case "/personas/detail":
		return c.renderPersonaDetail(ctx, s, params)
	case "/personas/create":
		return c.renderPersonaForm(ctx, s, params)
	case "/personas/edit":
		return c.renderPersonaForm(ctx, s, params)
	case "/skills":
		return c.renderSkills(ctx, s, params)
	case "/skills/detail":
		return c.renderSkillDetail(ctx, s, params)
	case "/skills/create":
		return c.renderSkillForm(ctx, s, params)
	case "/skills/edit":
		return c.renderSkillForm(ctx, s, params)
	case "/traits":
		return c.renderTraits(ctx, s, params)
	case "/traits/detail":
		return c.renderTraitDetail(ctx, s, params)
	case "/traits/create":
		return c.renderTraitForm(ctx, s, params)
	case "/traits/edit":
		return c.renderTraitForm(ctx, s, params)
	case "/behaviors":
		return c.renderBehaviors(ctx, s, params)
	case "/behaviors/detail":
		return c.renderBehaviorDetail(ctx, s, params)
	case "/behaviors/create":
		return c.renderBehaviorForm(ctx, s, params)
	case "/behaviors/edit":
		return c.renderBehaviorForm(ctx, s, params)
	case "/runs":
		return c.renderRuns(ctx, s, params)
	case "/runs/detail":
		return c.renderRunDetail(ctx, s, params)
	case "/checkpoints":
		return c.renderCheckpoints(ctx, s, params)
	case "/checkpoints/detail":
		return c.renderCheckpointDetail(ctx, s, params)
	case "/tools":
		return c.renderTools(ctx, s, params)
	case "/tools/detail":
		return c.renderToolDetail(ctx, s, params)
	case "/models":
		return c.renderModels(ctx, params)
	case "/models/detail":
		return c.renderModelDetail(ctx, params)
	case "/knowledge":
		return c.renderKnowledge(ctx, params)
	case "/knowledge/detail":
		return c.renderKnowledgeDetail(ctx, params)
	case "/safety/profiles":
		return c.renderSafetyProfiles(ctx, params)
	case "/safety/scans":
		return c.renderSafetyScans(ctx, params)
	case "/memory":
		return c.renderMemory(ctx, s, params)
	case "/chat":
		return c.renderChat(ctx, s, params)
	case "/playground":
		return c.renderPlayground(ctx, s, params)
	case "/playground/preview-prompt":
		return c.renderPlaygroundPromptPreview(ctx, s, params)
	default:
		return components.EmptyState("alert-circle", "Page not found", "The requested page '"+pageRoute+"' does not exist in the Cortex dashboard."), nil
	}
}

// RenderWidget renders a widget by ID.
func (c *Contributor) RenderWidget(ctx context.Context, widgetID string) (templ.Component, error) {
	if c.engine == nil {
		return nil, contributor.ErrWidgetNotFound
	}
	s := c.engine.Store()
	if s == nil {
		return nil, contributor.ErrWidgetNotFound
	}

	for _, dp := range c.dashboardPlugins() {
		for _, w := range dp.DashboardWidgets(ctx) {
			if w.ID == widgetID {
				return w.Render(ctx), nil
			}
		}
	}

	switch widgetID {
	case "cortex-stats":
		return c.renderStatsWidget(ctx, s)
	case "cortex-recent-runs":
		return c.renderRecentRunsWidget(ctx, s)
	case "cortex-active-checkpoints":
		return c.renderActiveCheckpointsWidget(ctx, s)
	case "cortex-safety-stats":
		return c.renderSafetyStatsWidget(ctx)
	case "cortex-knowledge-stats":
		return c.renderKnowledgeStatsWidget(ctx)
	default:
		return nil, contributor.ErrWidgetNotFound
	}
}

// RenderSettings renders a settings panel by ID.
func (c *Contributor) RenderSettings(ctx context.Context, settingID string) (templ.Component, error) {
	pluginSettings := c.collectPluginSettings(ctx)

	switch settingID {
	case "cortex-config":
		return c.renderSettings(ctx, pluginSettings)
	default:
		return nil, contributor.ErrSettingNotFound
	}
}

// --- Page Renderers ---

func (c *Contributor) renderOverview(ctx context.Context, s store.Store) (templ.Component, error) {
	counts := fetchEntityCounts(ctx, s, "")
	runs, _ := fetchRecentRuns(ctx, s, 10)       //nolint:errcheck // best-effort UI data
	cps, _ := fetchPendingCheckpoints(ctx, s, 5) //nolint:errcheck // best-effort UI data
	agentNames := buildAgentNameMap(ctx, s, "")
	pluginSections := c.collectPluginSections(ctx)

	return templ.ComponentFunc(func(tCtx context.Context, w io.Writer) error {
		childCtx := templ.WithChildren(tCtx, components.PluginSections(pluginSections))
		return pages.OverviewPage(counts.Agents, counts.Personas, counts.Skills, counts.Traits, counts.Behaviors, counts.Runs, counts.Checkpoints, runs, cps, agentNames).Render(childCtx, w)
	}), nil
}

func (c *Contributor) renderAgents(ctx context.Context, s store.Store, params contributor.Params) (templ.Component, error) {
	search := params.QueryParams["search"]
	limit := parseIntParam(params.QueryParams, "limit", 20)
	offset := parseIntParam(params.QueryParams, "offset", 0)
	items, total, err := fetchAgentsPaginated(ctx, s, "", search, limit, offset)
	if err != nil {
		items = nil
		total = 0
	}
	pg := NewPaginationMeta(total, limit, offset)
	return pages.AgentsPage(items, search, pg), nil
}

func (c *Contributor) renderAgentDetail(ctx context.Context, s store.Store, params contributor.Params) (templ.Component, error) {
	name := params.QueryParams["name"]
	if name == "" {
		return nil, contributor.ErrPageNotFound
	}
	ag, err := s.GetByName(ctx, "", name)
	if err != nil {
		return nil, fmt.Errorf("dashboard: resolve agent: %w", err)
	}
	recentRuns, _ := s.ListRuns(ctx, &run.ListFilter{AgentID: ag.ID.String(), Limit: 10}) //nolint:errcheck // best-effort UI data
	totalRuns, _ := s.CountRuns(ctx, &run.ListFilter{AgentID: ag.ID.String()})            //nolint:errcheck // best-effort UI data
	var successRate float64
	if totalRuns > 0 {
		completedRuns, _ := s.CountRuns(ctx, &run.ListFilter{AgentID: ag.ID.String(), State: run.StateCompleted}) //nolint:errcheck // best-effort UI data
		successRate = float64(completedRuns) / float64(totalRuns) * 100
	}
	var lastRunTime string
	if len(recentRuns) > 0 {
		lastRunTime = formatTimeAgo(recentRuns[0].CreatedAt)
	} else {
		lastRunTime = "Never"
	}
	pluginSections := c.collectAgentDetailSections(ctx, ag.ID)

	return templ.ComponentFunc(func(tCtx context.Context, w io.Writer) error {
		childCtx := templ.WithChildren(tCtx, components.PluginSections(pluginSections))
		return pages.AgentDetailPage(ag, recentRuns, totalRuns, successRate, lastRunTime).Render(childCtx, w)
	}), nil
}

func (c *Contributor) renderAgentForm(ctx context.Context, s store.Store, params contributor.Params) (templ.Component, error) {
	personas, _ := fetchPersonas(ctx, s, "")   //nolint:errcheck // best-effort UI data
	skills, _ := fetchSkills(ctx, s, "")       //nolint:errcheck // best-effort UI data
	traits, _ := fetchTraits(ctx, s, "")       //nolint:errcheck // best-effort UI data
	behaviors, _ := fetchBehaviors(ctx, s, "") //nolint:errcheck // best-effort UI data

	name := params.QueryParams["name"]
	if name != "" {
		ag, err := s.GetByName(ctx, "", name)
		if err != nil {
			return nil, fmt.Errorf("dashboard: resolve agent for edit: %w", err)
		}
		return pages.AgentFormPage(ag, personas, skills, traits, behaviors), nil
	}
	return pages.AgentFormPage(nil, personas, skills, traits, behaviors), nil
}

func (c *Contributor) renderPersonas(ctx context.Context, s store.Store, params contributor.Params) (templ.Component, error) {
	search := params.QueryParams["search"]
	limit := parseIntParam(params.QueryParams, "limit", 20)
	offset := parseIntParam(params.QueryParams, "offset", 0)
	items, total, err := fetchPersonasPaginated(ctx, s, "", search, limit, offset)
	if err != nil {
		items = nil
		total = 0
	}
	pg := NewPaginationMeta(total, limit, offset)
	return pages.PersonasPage(items, search, pg), nil
}

func (c *Contributor) renderPersonaDetail(ctx context.Context, s store.Store, params contributor.Params) (templ.Component, error) {
	name := params.QueryParams["name"]
	if name == "" {
		return nil, contributor.ErrPageNotFound
	}
	p, err := s.GetPersonaByName(ctx, "", name)
	if err != nil {
		return nil, fmt.Errorf("dashboard: resolve persona: %w", err)
	}
	pluginSections := c.collectPersonaDetailSections(ctx, p.ID)

	return templ.ComponentFunc(func(tCtx context.Context, w io.Writer) error {
		childCtx := templ.WithChildren(tCtx, components.PluginSections(pluginSections))
		return pages.PersonaDetailPage(p).Render(childCtx, w)
	}), nil
}

func (c *Contributor) renderPersonaForm(ctx context.Context, s store.Store, params contributor.Params) (templ.Component, error) {
	skills, _ := fetchSkills(ctx, s, "")       //nolint:errcheck // best-effort UI data
	traits, _ := fetchTraits(ctx, s, "")       //nolint:errcheck // best-effort UI data
	behaviors, _ := fetchBehaviors(ctx, s, "") //nolint:errcheck // best-effort UI data

	name := params.QueryParams["name"]
	if name != "" {
		p, err := s.GetPersonaByName(ctx, "", name)
		if err != nil {
			return nil, fmt.Errorf("dashboard: resolve persona for edit: %w", err)
		}
		return pages.PersonaFormPage(p, skills, traits, behaviors), nil
	}
	return pages.PersonaFormPage(nil, skills, traits, behaviors), nil
}

func (c *Contributor) renderSkills(ctx context.Context, s store.Store, params contributor.Params) (templ.Component, error) {
	search := params.QueryParams["search"]
	limit := parseIntParam(params.QueryParams, "limit", 20)
	offset := parseIntParam(params.QueryParams, "offset", 0)
	items, total, err := fetchSkillsPaginated(ctx, s, "", search, limit, offset)
	if err != nil {
		items = nil
		total = 0
	}
	pg := NewPaginationMeta(total, limit, offset)
	return pages.SkillsPage(items, search, pg), nil
}

func (c *Contributor) renderSkillDetail(ctx context.Context, s store.Store, params contributor.Params) (templ.Component, error) {
	name := params.QueryParams["name"]
	if name == "" {
		return nil, contributor.ErrPageNotFound
	}
	sk, err := s.GetSkillByName(ctx, "", name)
	if err != nil {
		return nil, fmt.Errorf("dashboard: resolve skill: %w", err)
	}
	agents, _ := fetchAgents(ctx, s, "")     //nolint:errcheck // best-effort UI data
	personas, _ := fetchPersonas(ctx, s, "") //nolint:errcheck // best-effort UI data
	usageCount := countUsageInAgents(agents, func(a *agent.Config) []string { return a.InlineSkills }, sk.Name) +
		countSkillUsageInPersonas(personas, sk.Name)
	return pages.SkillDetailPage(sk, usageCount), nil
}

func (c *Contributor) renderSkillForm(_ context.Context, s store.Store, params contributor.Params) (templ.Component, error) {
	name := params.QueryParams["name"]
	if name != "" {
		sk, err := s.GetSkillByName(context.Background(), "", name)
		if err != nil {
			return nil, fmt.Errorf("dashboard: resolve skill for edit: %w", err)
		}
		return pages.SkillFormPage(sk), nil
	}
	return pages.SkillFormPage(nil), nil
}

func (c *Contributor) renderTraits(ctx context.Context, s store.Store, params contributor.Params) (templ.Component, error) {
	search := params.QueryParams["search"]
	category := trait.Category(params.QueryParams["category"])
	limit := parseIntParam(params.QueryParams, "limit", 20)
	offset := parseIntParam(params.QueryParams, "offset", 0)
	items, total, err := fetchTraitsPaginated(ctx, s, "", search, category, limit, offset)
	if err != nil {
		items = nil
		total = 0
	}
	pg := NewPaginationMeta(total, limit, offset)
	return pages.TraitsPage(items, search, string(category), pg), nil
}

func (c *Contributor) renderTraitDetail(ctx context.Context, s store.Store, params contributor.Params) (templ.Component, error) {
	name := params.QueryParams["name"]
	if name == "" {
		return nil, contributor.ErrPageNotFound
	}
	t, err := s.GetTraitByName(ctx, "", name)
	if err != nil {
		return nil, fmt.Errorf("dashboard: resolve trait: %w", err)
	}
	agents, _ := fetchAgents(ctx, s, "")     //nolint:errcheck // best-effort UI data
	personas, _ := fetchPersonas(ctx, s, "") //nolint:errcheck // best-effort UI data
	usageCount := countUsageInAgents(agents, func(a *agent.Config) []string { return a.InlineTraits }, t.Name) +
		countTraitUsageInPersonas(personas, t.Name)
	return pages.TraitDetailPage(t, usageCount), nil
}

func (c *Contributor) renderTraitForm(_ context.Context, s store.Store, params contributor.Params) (templ.Component, error) {
	name := params.QueryParams["name"]
	if name != "" {
		t, err := s.GetTraitByName(context.Background(), "", name)
		if err != nil {
			return nil, fmt.Errorf("dashboard: resolve trait for edit: %w", err)
		}
		return pages.TraitFormPage(t), nil
	}
	return pages.TraitFormPage(nil), nil
}

func (c *Contributor) renderBehaviors(ctx context.Context, s store.Store, params contributor.Params) (templ.Component, error) {
	search := params.QueryParams["search"]
	limit := parseIntParam(params.QueryParams, "limit", 20)
	offset := parseIntParam(params.QueryParams, "offset", 0)
	items, total, err := fetchBehaviorsPaginated(ctx, s, "", search, limit, offset)
	if err != nil {
		items = nil
		total = 0
	}
	pg := NewPaginationMeta(total, limit, offset)
	return pages.BehaviorsPage(items, search, pg), nil
}

func (c *Contributor) renderBehaviorDetail(ctx context.Context, s store.Store, params contributor.Params) (templ.Component, error) {
	name := params.QueryParams["name"]
	if name == "" {
		return nil, contributor.ErrPageNotFound
	}
	b, err := s.GetBehaviorByName(ctx, "", name)
	if err != nil {
		return nil, fmt.Errorf("dashboard: resolve behavior: %w", err)
	}
	agents, _ := fetchAgents(ctx, s, "")     //nolint:errcheck // best-effort UI data
	personas, _ := fetchPersonas(ctx, s, "") //nolint:errcheck // best-effort UI data
	usageCount := countUsageInAgents(agents, func(a *agent.Config) []string { return a.InlineBehaviors }, b.Name) +
		countBehaviorUsageInPersonas(personas, b.Name)
	return pages.BehaviorDetailPage(b, usageCount), nil
}

func (c *Contributor) renderBehaviorForm(_ context.Context, s store.Store, params contributor.Params) (templ.Component, error) {
	name := params.QueryParams["name"]
	if name != "" {
		b, err := s.GetBehaviorByName(context.Background(), "", name)
		if err != nil {
			return nil, fmt.Errorf("dashboard: resolve behavior for edit: %w", err)
		}
		return pages.BehaviorFormPage(b), nil
	}
	return pages.BehaviorFormPage(nil), nil
}

func (c *Contributor) renderRuns(ctx context.Context, s store.Store, params contributor.Params) (templ.Component, error) {
	agentFilter := params.QueryParams["agent"]
	stateFilter := params.QueryParams["state"]
	limit := parseIntParam(params.QueryParams, "limit", 20)
	offset := parseIntParam(params.QueryParams, "offset", 0)
	items, total, err := fetchRunsPaginated(ctx, s, agentFilter, stateFilter, limit, offset)
	if err != nil {
		items = nil
		total = 0
	}
	agents, _ := fetchAgents(ctx, s, "") //nolint:errcheck // best-effort UI data
	agentNames := buildAgentNameMapFrom(agents)
	pg := NewPaginationMeta(total, limit, offset)
	return pages.RunsPage(items, agentFilter, stateFilter, agents, agentNames, pg), nil
}

func (c *Contributor) renderRunDetail(ctx context.Context, s store.Store, params contributor.Params) (templ.Component, error) {
	idStr := params.QueryParams["id"]
	if idStr == "" {
		return nil, contributor.ErrPageNotFound
	}
	runID, err := id.ParseAgentRunID(idStr)
	if err != nil {
		return nil, contributor.ErrPageNotFound
	}
	r, err := s.GetRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("dashboard: resolve run: %w", err)
	}
	steps, _ := s.ListSteps(ctx, runID) //nolint:errcheck // best-effort UI data
	stepToolCalls := make(map[string][]*run.ToolCall)
	totalToolCalls := 0
	for _, step := range steps {
		tcs, _ := s.ListToolCalls(ctx, step.ID) //nolint:errcheck // best-effort UI data
		stepToolCalls[step.ID.String()] = tcs
		totalToolCalls += len(tcs)
	}
	agentNames := buildAgentNameMap(ctx, s, "")
	agentName := resolveAgentName(r.AgentID.String(), agentNames)
	duration := formatDuration(r.StartedAt, r.CompletedAt)
	pluginSections := c.collectRunDetailSections(ctx, runID)

	return templ.ComponentFunc(func(tCtx context.Context, w io.Writer) error {
		childCtx := templ.WithChildren(tCtx, components.PluginSections(pluginSections))
		return pages.RunDetailPage(r, steps, stepToolCalls, agentName, totalToolCalls, duration).Render(childCtx, w)
	}), nil
}

func (c *Contributor) renderCheckpoints(ctx context.Context, s store.Store, params contributor.Params) (templ.Component, error) {
	limit := parseIntParam(params.QueryParams, "limit", 20)
	offset := parseIntParam(params.QueryParams, "offset", 0)
	items, total, err := fetchCheckpointsPaginated(ctx, s, limit, offset)
	if err != nil {
		items = nil
		total = 0
	}
	pg := NewPaginationMeta(total, limit, offset)
	return pages.CheckpointsPage(items, pg), nil
}

func (c *Contributor) renderCheckpointDetail(ctx context.Context, s store.Store, params contributor.Params) (templ.Component, error) {
	idStr := params.QueryParams["id"]
	if idStr == "" {
		return nil, contributor.ErrPageNotFound
	}
	cpID, err := id.ParseCheckpointID(idStr)
	if err != nil {
		return nil, contributor.ErrPageNotFound
	}
	cp, err := s.GetCheckpoint(ctx, cpID)
	if err != nil {
		return nil, fmt.Errorf("dashboard: resolve checkpoint: %w", err)
	}
	agentNames := buildAgentNameMap(ctx, s, "")
	agentName := resolveAgentName(cp.AgentID.String(), agentNames)
	return pages.CheckpointDetailPage(cp, agentName), nil
}

func (c *Contributor) renderTools(ctx context.Context, s store.Store, params contributor.Params) (templ.Component, error) {
	search := params.QueryParams["search"]
	limit := parseIntParam(params.QueryParams, "limit", 20)
	offset := parseIntParam(params.QueryParams, "offset", 0)
	items, total := discoverToolsPaginated(ctx, s, "", search, limit, offset)
	pg := NewPaginationMeta(total, limit, offset)
	return pages.ToolsPage(items, search, pg), nil
}

func (c *Contributor) renderToolDetail(ctx context.Context, s store.Store, params contributor.Params) (templ.Component, error) {
	name := params.QueryParams["name"]
	if name == "" {
		return nil, contributor.ErrPageNotFound
	}
	tool, err := discoverToolByName(ctx, s, "", name)
	if err != nil {
		return nil, fmt.Errorf("dashboard: resolve tool: %w", err)
	}
	return pages.ToolDetailPage(tool), nil
}

func (c *Contributor) renderModels(ctx context.Context, params contributor.Params) (templ.Component, error) {
	if c.modelSource == nil {
		return components.EmptyState("sparkles", "No model source", "No LLM gateway is configured. Register a Nexus extension to discover models."), nil
	}
	search := params.QueryParams["search"]
	providerFilter := params.QueryParams["provider"]
	limit := parseIntParam(params.QueryParams, "limit", 20)
	offset := parseIntParam(params.QueryParams, "offset", 0)

	allModels, _ := c.modelSource.ListModels(ctx)    //nolint:errcheck // best-effort UI data
	providers, _ := c.modelSource.ListProviders(ctx) //nolint:errcheck // best-effort UI data

	// Filter by search and provider.
	filtered := make([]ModelInfo, 0, len(allModels))
	for _, m := range allModels {
		if providerFilter != "" && m.Provider != providerFilter {
			continue
		}
		if search != "" && !containsInsensitive(m.Name, search) && !containsInsensitive(m.ID, search) {
			continue
		}
		filtered = append(filtered, m)
	}

	total := int64(len(filtered))
	if offset >= len(filtered) {
		filtered = nil
	} else {
		end := offset + limit
		if end > len(filtered) {
			end = len(filtered)
		}
		filtered = filtered[offset:end]
	}

	pg := NewPaginationMeta(total, limit, offset)
	return pages.ModelsPage(filtered, providers, search, providerFilter, pg), nil
}

func (c *Contributor) renderModelDetail(ctx context.Context, params contributor.Params) (templ.Component, error) {
	if c.modelSource == nil {
		return components.EmptyState("sparkles", "No model source", "No LLM gateway is configured."), nil
	}
	modelID := params.QueryParams["id"]
	if modelID == "" {
		return nil, contributor.ErrPageNotFound
	}
	models, err := c.modelSource.ListModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("dashboard: list models: %w", err)
	}
	for i := range models {
		if models[i].ID == modelID {
			return pages.ModelDetailPage(&models[i]), nil
		}
	}
	return nil, contributor.ErrPageNotFound
}

func (c *Contributor) renderMemory(ctx context.Context, s store.Store, params contributor.Params) (templ.Component, error) {
	agents, _ := fetchAgents(ctx, s, "") //nolint:errcheck // best-effort UI data
	agentIDStr := params.QueryParams["agent"]
	var messages []memory.Message
	if agentIDStr != "" {
		agID, parseErr := id.ParseAgentID(agentIDStr)
		if parseErr == nil {
			messages, _ = s.LoadConversation(ctx, agID, "", 100) //nolint:errcheck // best-effort UI data
		}
	}
	return pages.MemoryPage(agents, agentIDStr, messages), nil
}

// --- Widget Renderers ---

func (c *Contributor) renderStatsWidget(ctx context.Context, s store.Store) (templ.Component, error) {
	counts := fetchEntityCounts(ctx, s, "")
	return widgets.StatsWidget(counts.Agents, counts.Personas, counts.Skills, counts.Traits, counts.Behaviors, counts.Runs, counts.Checkpoints), nil
}

func (c *Contributor) renderRecentRunsWidget(ctx context.Context, s store.Store) (templ.Component, error) {
	runs, _ := fetchRecentRuns(ctx, s, 10) //nolint:errcheck // best-effort UI data
	agentNames := buildAgentNameMap(ctx, s, "")
	return widgets.RecentRunsWidget(runs, agentNames), nil
}

func (c *Contributor) renderActiveCheckpointsWidget(ctx context.Context, s store.Store) (templ.Component, error) {
	cps, _ := fetchPendingCheckpoints(ctx, s, 5) //nolint:errcheck // best-effort UI data
	return widgets.ActiveCheckpointsWidget(cps), nil
}

// --- Settings Renderer ---

func (c *Contributor) renderSettings(_ context.Context, pluginSettings []templ.Component) (templ.Component, error) {
	if c.engine == nil {
		return nil, contributor.ErrSettingNotFound
	}
	cfg := c.engine.Config()
	pluginNames := make([]string, 0, len(c.plugins))
	for _, p := range c.plugins {
		pluginNames = append(pluginNames, p.Name())
	}

	return templ.ComponentFunc(func(tCtx context.Context, w io.Writer) error {
		childCtx := templ.WithChildren(tCtx, components.PluginSections(pluginSettings))
		return settings.ConfigPanel(cfg, pluginNames).Render(childCtx, w)
	}), nil
}

// --- Plugin Helpers ---

func (c *Contributor) dashboardPlugins() []Plugin {
	var dps []Plugin
	for _, p := range c.plugins {
		if dp, ok := p.(Plugin); ok {
			dps = append(dps, dp)
		}
	}
	return dps
}

func (c *Contributor) collectPluginSections(ctx context.Context) []templ.Component {
	var sections []templ.Component
	for _, dp := range c.dashboardPlugins() {
		for _, w := range dp.DashboardWidgets(ctx) {
			sections = append(sections, w.Render(ctx))
		}
	}
	return sections
}

func (c *Contributor) collectPluginSettings(ctx context.Context) []templ.Component {
	var panels []templ.Component
	for _, dp := range c.dashboardPlugins() {
		if panel := dp.DashboardSettingsPanel(ctx); panel != nil {
			panels = append(panels, panel)
		}
	}
	return panels
}

func (c *Contributor) collectAgentDetailSections(ctx context.Context, agentID id.AgentID) []templ.Component {
	var sections []templ.Component
	for _, p := range c.plugins {
		if adc, ok := p.(AgentDetailContributor); ok {
			if section := adc.DashboardAgentDetailSection(ctx, agentID); section != nil {
				sections = append(sections, section)
			}
		}
	}
	return sections
}

func (c *Contributor) collectPersonaDetailSections(ctx context.Context, personaID id.PersonaID) []templ.Component {
	var sections []templ.Component
	for _, p := range c.plugins {
		if pdc, ok := p.(PersonaDetailContributor); ok {
			if section := pdc.DashboardPersonaDetailSection(ctx, personaID); section != nil {
				sections = append(sections, section)
			}
		}
	}
	return sections
}

func (c *Contributor) collectRunDetailSections(ctx context.Context, runID id.AgentRunID) []templ.Component {
	var sections []templ.Component
	for _, p := range c.plugins {
		if rdc, ok := p.(RunDetailContributor); ok {
			if section := rdc.DashboardRunDetailSection(ctx, runID); section != nil {
				sections = append(sections, section)
			}
		}
	}
	return sections
}

// --- Chat & Playground Renderers ---

func (c *Contributor) renderChat(ctx context.Context, s store.Store, params contributor.Params) (templ.Component, error) {
	agents, _ := fetchAgents(ctx, s, "") //nolint:errcheck // best-effort UI data
	selectedAgent := params.QueryParams["agent"]

	var messages []memory.Message
	if selectedAgent != "" {
		ag, err := s.GetByName(ctx, "", selectedAgent)
		if err == nil {
			messages, _ = s.LoadConversation(ctx, ag.ID, "", 100) //nolint:errcheck // best-effort UI data
		}
	}

	pluginSections := c.collectChatContributions(ctx)
	return templ.ComponentFunc(func(tCtx context.Context, w io.Writer) error {
		childCtx := templ.WithChildren(tCtx, components.PluginSections(pluginSections))
		return pages.ChatPage(agents, selectedAgent, messages).Render(childCtx, w)
	}), nil
}

func (c *Contributor) renderPlayground(ctx context.Context, s store.Store, params contributor.Params) (templ.Component, error) {
	agents, _ := fetchAgents(ctx, s, "")       //nolint:errcheck // best-effort UI data
	personas, _ := fetchPersonas(ctx, s, "")   //nolint:errcheck // best-effort UI data
	skills, _ := fetchSkills(ctx, s, "")       //nolint:errcheck // best-effort UI data
	traits, _ := fetchTraits(ctx, s, "")       //nolint:errcheck // best-effort UI data
	behaviors, _ := fetchBehaviors(ctx, s, "") //nolint:errcheck // best-effort UI data
	engineCfg := c.engine.Config()

	selectedAgent := params.QueryParams["agent"]

	pluginSections := c.collectPlaygroundContributions(ctx)
	return templ.ComponentFunc(func(tCtx context.Context, w io.Writer) error {
		childCtx := templ.WithChildren(tCtx, components.PluginSections(pluginSections))
		return pages.PlaygroundPage(agents, personas, skills, traits, behaviors, engineCfg, selectedAgent).Render(childCtx, w)
	}), nil
}

func (c *Contributor) renderPlaygroundPromptPreview(ctx context.Context, s store.Store, params contributor.Params) (templ.Component, error) {
	agentName := params.QueryParams["agent"]
	prompt := computeSystemPrompt(ctx, s, agentName, params.QueryParams["persona"], params.QueryParams["skills"], params.QueryParams["traits"])
	return pages.PlaygroundPromptPreview(prompt), nil
}

func (c *Contributor) collectChatContributions(ctx context.Context) []templ.Component {
	var sections []templ.Component
	for _, p := range c.plugins {
		if cc, ok := p.(ChatContributor); ok {
			if toolbar := cc.DashboardChatToolbar(ctx); toolbar != nil {
				sections = append(sections, toolbar)
			}
		}
	}
	return sections
}

func (c *Contributor) collectPlaygroundContributions(ctx context.Context) []templ.Component {
	var sections []templ.Component
	for _, p := range c.plugins {
		if pc, ok := p.(PlaygroundContributor); ok {
			if panel := pc.DashboardPlaygroundPanel(ctx); panel != nil {
				sections = append(sections, panel)
			}
		}
	}
	return sections
}

// --- Knowledge Renderers ---

func (c *Contributor) renderKnowledge(ctx context.Context, params contributor.Params) (templ.Component, error) {
	if c.knowledgeSource == nil {
		return components.EmptyState("book-open", "No knowledge source", "Weave is not configured. Register the Weave extension to enable knowledge management."), nil
	}
	collections, err := c.knowledgeSource.ListCollections(ctx)
	if err != nil {
		return nil, fmt.Errorf("dashboard: list knowledge collections: %w", err)
	}

	// Client-side search filter.
	search := params.QueryParams["search"]
	if search != "" {
		filtered := make([]KnowledgeCollectionInfo, 0, len(collections))
		for _, col := range collections {
			if containsInsensitive(col.Name, search) || containsInsensitive(col.ID, search) {
				filtered = append(filtered, col)
			}
		}
		collections = filtered
	}

	return pages.KnowledgePage(collections, search), nil
}

func (c *Contributor) renderKnowledgeDetail(ctx context.Context, params contributor.Params) (templ.Component, error) {
	if c.knowledgeSource == nil {
		return components.EmptyState("book-open", "No knowledge source", "Weave is not configured."), nil
	}
	colID := params.QueryParams["id"]
	if colID == "" {
		return nil, contributor.ErrPageNotFound
	}
	stats, err := c.knowledgeSource.CollectionStats(ctx, colID)
	if err != nil {
		return nil, fmt.Errorf("dashboard: knowledge collection stats: %w", err)
	}
	return pages.KnowledgeDetailPage(stats), nil
}

func (c *Contributor) renderKnowledgeStatsWidget(ctx context.Context) (templ.Component, error) {
	if c.knowledgeSource == nil {
		return nil, contributor.ErrWidgetNotFound
	}
	collections, err := c.knowledgeSource.ListCollections(ctx)
	if err != nil {
		collections = nil
	}
	var totalDocs, totalChunks int64
	for _, col := range collections {
		totalDocs += col.DocumentCount
		totalChunks += col.ChunkCount
	}
	return widgets.KnowledgeStatsWidget(int64(len(collections)), totalDocs, totalChunks), nil
}

// --- Safety Renderers ---

func (c *Contributor) renderSafetyProfiles(ctx context.Context, _ contributor.Params) (templ.Component, error) {
	if c.safetySource == nil {
		return components.EmptyState("shield-check", "No safety source", "Shield is not configured. Register the Shield extension to enable safety profiles."), nil
	}
	profiles, err := c.safetySource.ListProfiles(ctx)
	if err != nil {
		return nil, fmt.Errorf("dashboard: list safety profiles: %w", err)
	}
	summary, _ := c.safetySource.GetScanSummary(ctx, "") //nolint:errcheck // best-effort UI data
	return pages.SafetyProfilesPage(profiles, summary), nil
}

func (c *Contributor) renderSafetyScans(ctx context.Context, params contributor.Params) (templ.Component, error) {
	if c.safetySource == nil {
		return components.EmptyState("scan-line", "No safety source", "Shield is not configured. Register the Shield extension to enable safety scanning."), nil
	}
	runID := params.QueryParams["run"]
	scans, err := c.safetySource.GetRunScans(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("dashboard: list safety scans: %w", err)
	}
	summary, _ := c.safetySource.GetScanSummary(ctx, "") //nolint:errcheck // best-effort UI data
	return pages.SafetyScansPage(scans, summary, runID), nil
}

func (c *Contributor) renderSafetyStatsWidget(ctx context.Context) (templ.Component, error) {
	if c.safetySource == nil {
		return nil, contributor.ErrWidgetNotFound
	}
	summary, err := c.safetySource.GetScanSummary(ctx, "")
	if err != nil {
		summary = &ScanSummary{}
	}
	return widgets.SafetyStatsWidget(summary), nil
}
