package orchestration

import (
	"sort"
	"strings"
)

// ExecutionRoster returns every local agent the configured strategy can invoke,
// including routing and synthesis agents. It validates the bounded participant
// list without running a strategy or resolving agent implementations.
func ExecutionRoster(cfg *Config) ([]string, error) {
	if cfg == nil || len(cfg.Participants) == 0 || len(cfg.Participants) > 256 || cfg.Settings.MaxConcurrency < 0 || cfg.Settings.MaxConcurrency > 256 || cfg.Settings.Rounds < 0 || cfg.Settings.Rounds > 128 {
		return nil, invalidPlan("invalid prepared orchestration limits")
	}
	strategy, err := Build(cfg.Strategy, nil, cfg.Participants, cfg.Settings)
	if err != nil {
		return nil, err
	}
	names := make(map[string]bool, len(cfg.Participants)+1)
	for _, part := range cfg.Participants {
		if !localRosterName(part.AgentName) || names[part.AgentName] {
			return nil, invalidPlan("invalid participant roster")
		}
		names[part.AgentName] = true
	}
	actor := ""
	switch selected := strategy.(type) {
	case *parallel:
		actor = cfg.Settings.Aggregator
	case *router:
		if len(cfg.Settings.RouterRules) == 0 {
			actor = cfg.Settings.RouterAgent
		}
		for _, target := range cfg.Settings.RouterRules {
			if !names[target] {
				return nil, invalidPlan("router target outside participants")
			}
		}
	case *hierarchical:
		actor = selected.resolveManager()
		if rosterErr := selected.validateRoster(actor, nonManagerParticipants(cfg.Participants, actor)); rosterErr != nil {
			return nil, rosterErr
		}
	case *debate:
		actor = selected.resolveJudge()
	}
	if actor != "" {
		if !localRosterName(actor) {
			return nil, invalidPlan("invalid strategy agent")
		}
		names[actor] = true
	}
	roster := make([]string, 0, len(names))
	for name := range names {
		roster = append(roster, name)
	}
	sort.Strings(roster)
	return roster, nil
}

func localRosterName(name string) bool {
	return name != "" && len(name) <= 256 && strings.TrimSpace(name) == name && !strings.ContainsAny(name, "@\x00\r\n")
}
