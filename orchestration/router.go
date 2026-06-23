package orchestration

import (
	"context"
	"sort"
	"strings"
)

type router struct {
	runner   AgentRunner
	appID    string
	parts    []Participant
	settings Settings
}

func newRouter(runner AgentRunner, appID string, parts []Participant, settings Settings) *router {
	return &router{runner: runner, appID: appID, parts: parts, settings: settings}
}

func (o *router) Strategy() string { return StrategyRouter }

func (o *router) Run(ctx context.Context, input string, bb *Blackboard) (*Result, error) {
	res := &Result{OrchestrationID: bb.OrchestrationID(), Strategy: StrategyRouter}
	if len(o.parts) == 0 {
		return res, nil
	}
	opts := runOptsFromSettings(o.settings)

	chosen := o.parts[0].AgentName
	routedBy := ""

	switch {
	case len(o.settings.RouterRules) > 0:
		lower := strings.ToLower(input)
		// Match rules in sorted-keyword order for deterministic tie-breaking
		// when an input matches more than one rule.
		keywords := make([]string, 0, len(o.settings.RouterRules))
		for k := range o.settings.RouterRules {
			keywords = append(keywords, k)
		}
		sort.Strings(keywords)
		for _, keyword := range keywords {
			if strings.Contains(lower, strings.ToLower(keyword)) {
				if findParticipant(o.parts, o.settings.RouterRules[keyword]) {
					chosen = o.settings.RouterRules[keyword]
				}
				break
			}
		}
	case o.settings.RouterAgent != "":
		prompt := buildRoutingPrompt(o.parts, input)
		ar, err := o.runner.RunAgent(ctx, o.appID, o.settings.RouterAgent, prompt, opts)
		if err != nil {
			res.Err = err
			return res, err
		}
		res.AgentResults = append(res.AgentResults, *ar)
		if name, ok := matchParticipant(o.parts, ar.Output); ok {
			chosen = name
		}
		routedBy = o.settings.RouterAgent
	}

	ar, err := o.runner.RunAgent(ctx, o.appID, chosen, composeInput(input, bb.Snapshot()), opts)
	if err != nil {
		res.Err = err
		return res, err
	}
	bb.Append(chosen, ar.Output)
	if routedBy != "" {
		bb.Handoff(ctx, routedBy, chosen, input)
	}
	res.AgentResults = append(res.AgentResults, *ar)
	res.Output = ar.Output
	res.Handoffs = bb.Handoffs()
	return res, nil
}

func buildRoutingPrompt(parts []Participant, input string) string {
	var sb strings.Builder
	sb.WriteString("You are a router. Choose the single best agent to handle the request.\n")
	sb.WriteString("Respond with ONLY the agent name, nothing else.\n\nAgents:\n")
	for _, p := range parts {
		if p.Role != "" {
			sb.WriteString("- " + p.AgentName + " (" + p.Role + ")\n")
		} else {
			sb.WriteString("- " + p.AgentName + "\n")
		}
	}
	sb.WriteString("\nRequest: " + input)
	return sb.String()
}

// matchParticipant finds the participant whose name appears in the router output.
func matchParticipant(parts []Participant, output string) (string, bool) {
	trimmed := strings.TrimSpace(output)
	// exact match first
	if findParticipant(parts, trimmed) {
		return trimmed, true
	}
	// substring match (router may add prose)
	lower := strings.ToLower(output)
	for _, p := range parts {
		if strings.Contains(lower, strings.ToLower(p.AgentName)) {
			return p.AgentName, true
		}
	}
	return "", false
}
