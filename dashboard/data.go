package dashboard

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/behavior"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/dashboard/shared"
	"github.com/xraph/cortex/persona"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/skill"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/trait"
)

// PaginationMeta is an alias for shared.PaginationMeta.
type PaginationMeta = shared.PaginationMeta

// NewPaginationMeta is a convenience re-export.
var NewPaginationMeta = shared.NewPaginationMeta

// --- Helper Functions ---

func parseIntParam(params map[string]string, key string, defaultVal int) int {
	v, ok := params[key]
	if !ok || v == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return defaultVal
	}
	return n
}

// --- Entity Counts ---

type entityCounts struct {
	Agents      int64
	Skills      int64
	Traits      int64
	Behaviors   int64
	Personas    int64
	Runs        int64
	Checkpoints int64
}

func fetchEntityCounts(ctx context.Context, s store.Store, appID string) entityCounts {
	var c entityCounts
	c.Agents, _ = s.CountAgents(ctx, &agent.ListFilter{AppID: appID})          //nolint:errcheck // best-effort UI data
	c.Skills, _ = s.CountSkills(ctx, &skill.ListFilter{AppID: appID})          //nolint:errcheck // best-effort UI data
	c.Traits, _ = s.CountTraits(ctx, &trait.ListFilter{AppID: appID})          //nolint:errcheck // best-effort UI data
	c.Behaviors, _ = s.CountBehaviors(ctx, &behavior.ListFilter{AppID: appID}) //nolint:errcheck // best-effort UI data
	c.Personas, _ = s.CountPersonas(ctx, &persona.ListFilter{AppID: appID})    //nolint:errcheck // best-effort UI data
	c.Runs, _ = s.CountRuns(ctx, &run.ListFilter{})                            //nolint:errcheck // best-effort UI data
	c.Checkpoints, _ = s.CountPending(ctx, &checkpoint.ListFilter{})           //nolint:errcheck // best-effort UI data
	return c
}

// --- Paginated Fetch Functions ---

func fetchAgentsPaginated(ctx context.Context, s store.Store, appID, search string, limit, offset int) ([]*agent.Config, int64, error) {
	filter := &agent.ListFilter{AppID: appID, Search: search, Limit: limit, Offset: offset}
	items, err := s.List(ctx, filter)
	if err != nil {
		return nil, 0, err
	}
	total, _ := s.CountAgents(ctx, &agent.ListFilter{AppID: appID, Search: search}) //nolint:errcheck // best-effort count
	return items, total, nil
}

func fetchSkillsPaginated(ctx context.Context, s store.Store, appID, search string, limit, offset int) ([]*skill.Skill, int64, error) {
	filter := &skill.ListFilter{AppID: appID, Search: search, Limit: limit, Offset: offset}
	items, err := s.ListSkills(ctx, filter)
	if err != nil {
		return nil, 0, err
	}
	total, _ := s.CountSkills(ctx, &skill.ListFilter{AppID: appID, Search: search}) //nolint:errcheck // best-effort count
	return items, total, nil
}

func fetchTraitsPaginated(ctx context.Context, s store.Store, appID, search string, category trait.Category, limit, offset int) ([]*trait.Trait, int64, error) {
	filter := &trait.ListFilter{AppID: appID, Search: search, Category: category, Limit: limit, Offset: offset}
	items, err := s.ListTraits(ctx, filter)
	if err != nil {
		return nil, 0, err
	}
	total, _ := s.CountTraits(ctx, &trait.ListFilter{AppID: appID, Search: search, Category: category}) //nolint:errcheck // best-effort count
	return items, total, nil
}

func fetchBehaviorsPaginated(ctx context.Context, s store.Store, appID, search string, limit, offset int) ([]*behavior.Behavior, int64, error) {
	filter := &behavior.ListFilter{AppID: appID, Search: search, Limit: limit, Offset: offset}
	items, err := s.ListBehaviors(ctx, filter)
	if err != nil {
		return nil, 0, err
	}
	total, _ := s.CountBehaviors(ctx, &behavior.ListFilter{AppID: appID, Search: search}) //nolint:errcheck // best-effort count
	return items, total, nil
}

func fetchPersonasPaginated(ctx context.Context, s store.Store, appID, search string, limit, offset int) ([]*persona.Persona, int64, error) {
	filter := &persona.ListFilter{AppID: appID, Search: search, Limit: limit, Offset: offset}
	items, err := s.ListPersonas(ctx, filter)
	if err != nil {
		return nil, 0, err
	}
	total, _ := s.CountPersonas(ctx, &persona.ListFilter{AppID: appID, Search: search}) //nolint:errcheck // best-effort count
	return items, total, nil
}

func fetchRunsPaginated(ctx context.Context, s store.Store, agentID, state string, limit, offset int) ([]*run.Run, int64, error) {
	filter := &run.ListFilter{AgentID: agentID, State: run.State(state), Limit: limit, Offset: offset}
	items, err := s.ListRuns(ctx, filter)
	if err != nil {
		return nil, 0, err
	}
	total, _ := s.CountRuns(ctx, &run.ListFilter{AgentID: agentID, State: run.State(state)}) //nolint:errcheck // best-effort count
	return items, total, nil
}

func fetchCheckpointsPaginated(ctx context.Context, s store.Store, limit, offset int) ([]*checkpoint.Checkpoint, int64, error) {
	filter := &checkpoint.ListFilter{Limit: limit, Offset: offset}
	items, err := s.ListPending(ctx, filter)
	if err != nil {
		return nil, 0, err
	}
	total, _ := s.CountPending(ctx, &checkpoint.ListFilter{}) //nolint:errcheck // best-effort count
	return items, total, nil
}

// --- Non-Paginated Fetch Functions ---

func fetchAgents(ctx context.Context, s store.Store, appID string) ([]*agent.Config, error) {
	return s.List(ctx, &agent.ListFilter{AppID: appID})
}

func fetchSkills(ctx context.Context, s store.Store, appID string) ([]*skill.Skill, error) {
	return s.ListSkills(ctx, &skill.ListFilter{AppID: appID})
}

func fetchTraits(ctx context.Context, s store.Store, appID string) ([]*trait.Trait, error) {
	return s.ListTraits(ctx, &trait.ListFilter{AppID: appID})
}

func fetchBehaviors(ctx context.Context, s store.Store, appID string) ([]*behavior.Behavior, error) {
	return s.ListBehaviors(ctx, &behavior.ListFilter{AppID: appID})
}

func fetchPersonas(ctx context.Context, s store.Store, _ string) ([]*persona.Persona, error) {
	return s.ListPersonas(ctx, &persona.ListFilter{AppID: ""})
}

func fetchRecentRuns(ctx context.Context, s store.Store, limit int) ([]*run.Run, error) {
	return s.ListRuns(ctx, &run.ListFilter{Limit: limit})
}

func fetchPendingCheckpoints(ctx context.Context, s store.Store, limit int) ([]*checkpoint.Checkpoint, error) {
	return s.ListPending(ctx, &checkpoint.ListFilter{Limit: limit})
}

// --- Formatting Helpers ---

func formatTimeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dmo ago", int(d.Hours()/(24*30)))
	default:
		return fmt.Sprintf("%dy ago", int(d.Hours()/(24*365)))
	}
}

// buildAgentNameMap fetches all agents and returns a map from AgentID.String() to agent Name.
func buildAgentNameMap(ctx context.Context, s store.Store, _ string) map[string]string {
	agents, err := s.List(ctx, &agent.ListFilter{AppID: ""})
	if err != nil {
		return map[string]string{}
	}
	m := make(map[string]string, len(agents))
	for _, a := range agents {
		m[a.ID.String()] = a.Name
	}
	return m
}

// buildAgentNameMapFrom builds a name map from an already-fetched agent list.
func buildAgentNameMapFrom(agents []*agent.Config) map[string]string {
	m := make(map[string]string, len(agents))
	for _, a := range agents {
		m[a.ID.String()] = a.Name
	}
	return m
}

// resolveAgentName returns the agent name for a given agent ID, or the raw ID if not found.
func resolveAgentName(agentIDStr string, nameMap map[string]string) string {
	if name, ok := nameMap[agentIDStr]; ok {
		return name
	}
	return agentIDStr
}

// formatDuration formats the duration between two optional time pointers.
func formatDuration(start, end *time.Time) string {
	if start == nil || end == nil {
		return "-"
	}
	d := end.Sub(*start)
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
}

// countUsageInAgents counts how many agents reference a given name in their inline lists.
func countUsageInAgents(agents []*agent.Config, fieldSelector func(*agent.Config) []string, name string) int {
	count := 0
	for _, a := range agents {
		for _, ref := range fieldSelector(a) {
			if ref == name {
				count++
				break
			}
		}
	}
	return count
}

// countUsageInPersonas counts how many personas reference a given name.
func countSkillUsageInPersonas(personas []*persona.Persona, name string) int {
	count := 0
	for _, p := range personas {
		for _, sa := range p.Skills {
			if sa.SkillName == name {
				count++
				break
			}
		}
	}
	return count
}

func countTraitUsageInPersonas(personas []*persona.Persona, name string) int {
	count := 0
	for _, p := range personas {
		for _, ta := range p.Traits {
			if ta.TraitName == name {
				count++
				break
			}
		}
	}
	return count
}

func countBehaviorUsageInPersonas(personas []*persona.Persona, name string) int {
	count := 0
	for _, p := range personas {
		for _, bName := range p.Behaviors {
			if bName == name {
				count++
				break
			}
		}
	}
	return count
}

// --- Tool Discovery ---

// DiscoveredTool is an alias for shared.DiscoveredTool.
type DiscoveredTool = shared.DiscoveredTool

// SkillToolRef is an alias for shared.SkillToolRef.
type SkillToolRef = shared.SkillToolRef

// discoverTools aggregates tool data from agents, skills, and recent runs.
func discoverTools(ctx context.Context, s store.Store, appID string) []DiscoveredTool {
	toolMap := make(map[string]*DiscoveredTool)

	// 1. Collect tools from agent configs.
	agents, _ := fetchAgents(ctx, s, appID) //nolint:errcheck // best-effort UI data
	for _, ag := range agents {
		for _, toolName := range ag.Tools {
			if toolName == "" {
				continue
			}
			dt := getOrCreateTool(toolMap, toolName)
			dt.AgentRefs = append(dt.AgentRefs, ag.Name)
		}
	}

	// 2. Collect tools from skill bindings.
	skills, _ := fetchSkills(ctx, s, appID) //nolint:errcheck // best-effort UI data
	for _, sk := range skills {
		for _, tb := range sk.Tools {
			if tb.ToolName == "" {
				continue
			}
			dt := getOrCreateTool(toolMap, tb.ToolName)
			dt.SkillBindings = append(dt.SkillBindings, SkillToolRef{
				SkillName:  sk.Name,
				Mastery:    string(tb.Mastery),
				Guidance:   tb.Guidance,
				PreferWhen: tb.PreferWhen,
			})
		}
	}

	// 3. Scan recent runs for tool call stats (capped for performance).
	recentRuns, _ := s.ListRuns(ctx, &run.ListFilter{Limit: 200}) //nolint:errcheck // best-effort UI data
	for _, r := range recentRuns {
		steps, _ := s.ListSteps(ctx, r.ID) //nolint:errcheck // best-effort UI data
		for _, step := range steps {
			tcs, _ := s.ListToolCalls(ctx, step.ID) //nolint:errcheck // best-effort UI data
			for _, tc := range tcs {
				if tc.ToolName == "" {
					continue
				}
				dt := getOrCreateTool(toolMap, tc.ToolName)
				dt.TotalCalls++
				if tc.Error != "" {
					dt.TotalErrors++
				}
				if tc.CompletedAt != nil {
					if dt.LastUsed == nil || tc.CompletedAt.After(*dt.LastUsed) {
						t := *tc.CompletedAt
						dt.LastUsed = &t
					}
				}
			}
		}
	}

	// Convert to sorted slice.
	result := make([]DiscoveredTool, 0, len(toolMap))
	for _, dt := range toolMap {
		result = append(result, *dt)
	}
	sortDiscoveredTools(result)
	return result
}

// discoverToolsPaginated returns a paginated, optionally searched, slice of tools.
func discoverToolsPaginated(ctx context.Context, s store.Store, appID, search string, limit, offset int) (items []DiscoveredTool, total int64) {
	all := discoverTools(ctx, s, appID)

	// Filter by search.
	if search != "" {
		filtered := make([]DiscoveredTool, 0)
		for _, dt := range all {
			if containsInsensitive(dt.Name, search) {
				filtered = append(filtered, dt)
			}
		}
		all = filtered
	}

	total = int64(len(all))

	// Paginate.
	if offset >= len(all) {
		return nil, total
	}
	end := offset + limit
	if end > len(all) {
		end = len(all)
	}
	return all[offset:end], total
}

// discoverToolByName returns a single tool's aggregated data.
func discoverToolByName(ctx context.Context, s store.Store, appID, toolName string) (*DiscoveredTool, error) {
	all := discoverTools(ctx, s, appID)
	for i := range all {
		if all[i].Name == toolName {
			return &all[i], nil
		}
	}
	return nil, fmt.Errorf("tool %q not found", toolName)
}

func getOrCreateTool(m map[string]*DiscoveredTool, name string) *DiscoveredTool {
	if dt, ok := m[name]; ok {
		return dt
	}
	dt := &DiscoveredTool{Name: name}
	m[name] = dt
	return dt
}

func sortDiscoveredTools(tools []DiscoveredTool) {
	// Sort by total references (agents + skills) descending, then name ascending.
	for i := 0; i < len(tools); i++ {
		for j := i + 1; j < len(tools); j++ {
			ri := len(tools[i].AgentRefs) + len(tools[i].SkillBindings) + tools[i].TotalCalls
			rj := len(tools[j].AgentRefs) + len(tools[j].SkillBindings) + tools[j].TotalCalls
			if rj > ri || (rj == ri && tools[j].Name < tools[i].Name) {
				tools[i], tools[j] = tools[j], tools[i]
			}
		}
	}
}

func containsInsensitive(s, substr string) bool {
	sl := make([]byte, len(s))
	for i := range s {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		sl[i] = c
	}
	subl := make([]byte, len(substr))
	for i := range substr {
		c := substr[i]
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		subl[i] = c
	}
	return bytesContains(sl, subl)
}

func bytesContains(s, sub []byte) bool {
	if len(sub) == 0 {
		return true
	}
	if len(sub) > len(s) {
		return false
	}
	for i := 0; i <= len(s)-len(sub); i++ {
		match := true
		for j := range sub {
			if s[i+j] != sub[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// --- System Prompt Computation ---

// computeSystemPrompt assembles the full system prompt from agent, persona, skills, and traits.
func computeSystemPrompt(ctx context.Context, s store.Store, agentName, personaRef, skillsCSV, traitsCSV string) string {
	var parts []string

	// Agent base prompt.
	if agentName != "" {
		ag, err := s.GetByName(ctx, "", agentName)
		if err == nil && ag.SystemPrompt != "" {
			parts = append(parts, ag.SystemPrompt)
		}
		// Use agent's persona ref if none explicitly provided.
		if personaRef == "" && ag != nil {
			personaRef = ag.PersonaRef
		}
	}

	// Persona identity.
	if personaRef != "" {
		p, err := s.GetPersonaByName(ctx, "", personaRef)
		if err == nil && p.Identity != "" {
			parts = append(parts, "\n## Identity\n"+p.Identity)
		}
	}

	// Skill prompt fragments.
	if skillsCSV != "" {
		for _, sName := range strings.Split(skillsCSV, ",") {
			sName = strings.TrimSpace(sName)
			if sName == "" {
				continue
			}
			sk, err := s.GetSkillByName(ctx, "", sName)
			if err == nil && sk.SystemPromptFragment != "" {
				parts = append(parts, "\n## Skill: "+sk.Name+"\n"+sk.SystemPromptFragment)
			}
		}
	}

	// Trait prompt injections.
	if traitsCSV != "" {
		for _, tName := range strings.Split(traitsCSV, ",") {
			tName = strings.TrimSpace(tName)
			if tName == "" {
				continue
			}
			t, err := s.GetTraitByName(ctx, "", tName)
			if err == nil {
				for _, inf := range t.Influences {
					if inf.Target == "prompt_injection" {
						if v, ok := inf.Value.(string); ok && v != "" {
							parts = append(parts, "\n## Trait: "+t.Name+"\n"+v)
						}
					}
				}
			}
		}
	}

	if len(parts) == 0 {
		return "(No system prompt configured)"
	}
	return strings.Join(parts, "\n")
}
