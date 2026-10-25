package optimization

import (
	"fmt"

	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/optimization/internal/canonical"
)

// OrchestrationPreset freezes a host-approved team, shared ceilings and editable
// participant sections. Sealing proves content integrity, not current authority.
type OrchestrationPreset struct {
	Name     string                        `json:"name"`
	Prepared *engine.PreparedOrchestration `json:"prepared"`
	Limits   execution.Resources           `json:"limits"`
	Guidance map[string][]string           `json:"guidance"`
	Digest   string                        `json:"digest"`
}

// OrchestrationSelection identifies one preset from the frozen edit policy.
// Prepared retains the selected team's complete immutable runtime snapshot.
type OrchestrationSelection struct {
	Preset   string                        `json:"preset"`
	Prepared *engine.PreparedOrchestration `json:"prepared"`
}

// SealOrchestrationPreset validates and detaches an approved configuration.
// The current runtime supports token ceilings and refuses unrestricted messaging.
func SealOrchestrationPreset(preset OrchestrationPreset) (OrchestrationPreset, error) {
	var empty OrchestrationPreset
	if !ValidOpaqueKey(preset.Name) || preset.Limits.Validate() != nil || preset.Limits.Steps < 1 || preset.Limits.Tokens < 1 || preset.Limits.FanOut < 1 || preset.Limits.FanOut > execution.MaxReservations || preset.Limits.Replans != 0 || preset.Limits.USDMicros != 0 {
		return empty, ErrInvalidSpec
	}
	view, err := preset.Prepared.Inspect()
	if err != nil {
		return empty, fmt.Errorf("%w: %w", ErrInvalidSpec, err)
	}
	if _, err = ScopeKey(view.Config.Scope); err != nil {
		return empty, err
	}
	for name, participant := range view.Participants {
		child, inspectErr := participant.Inspect()
		if inspectErr != nil {
			return empty, inspectErr
		}
		budget := child.Config.Budget
		tokens := budget.MaxTokens
		if child.Config.Verification != nil {
			tokens = child.Config.Verification.TotalTokens
		}
		if budget.Validate() != nil || budget.MaxSteps < 1 || budget.MaxTokens < 1 || budget.Deadline.IsZero() || len(child.Tools) > 0 && budget.MaxToolCalls < 1 || !preset.Limits.Covers(execution.Resources{Steps: int64(budget.MaxSteps), Tokens: int64(tokens), ToolCalls: int64(budget.MaxToolCalls), FanOut: 1}) {
			return empty, ErrInvalidSpec
		}
		for _, tool := range child.Tools {
			if tool.Name == "agent_send" || tool.Name == "agent_ask" || tool.Name == "agent_inbox" {
				return empty, ErrUnavailable
			}
		}
		if validationErr := uniqueNames(preset.Guidance[name]); validationErr != nil {
			return empty, validationErr
		}
		for _, sectionID := range preset.Guidance[name] {
			found := false
			for _, section := range child.Sections {
				if section.ID == sectionID && !section.Locked {
					found = true
				}
			}
			if !found {
				return empty, ErrPolicyDenied
			}
		}
	}
	for name := range preset.Guidance {
		if view.Participants[name] == nil {
			return empty, ErrPolicyDenied
		}
	}
	preset.Digest = ""
	data, err := canonical.Marshal(preset)
	if err != nil || len(data) > 16<<20 {
		return empty, ErrInvalidSpec
	}
	var detached OrchestrationPreset
	if decodeErr := canonical.Decode(data, &detached); decodeErr != nil {
		return empty, decodeErr
	}
	detached.Digest = digestBytes(data)
	return detached, nil
}

func validateOrchestrationCatalog(presets []OrchestrationPreset) error {
	if len(presets) > 16 {
		return ErrInvalidSpec
	}
	seen := make(map[string]bool, len(presets))
	for _, preset := range presets {
		sealed, err := SealOrchestrationPreset(preset)
		if err != nil {
			return err
		}
		if seen[preset.Name] || sealed.Digest != preset.Digest {
			return ErrStaleVersion
		}
		seen[preset.Name] = true
	}
	return nil
}

func orchestrationPreset(policy EditPolicy, name string) (OrchestrationPreset, error) {
	for _, preset := range policy.OrchestrationPresets {
		if preset.Name == name {
			return preset, nil
		}
	}
	return OrchestrationPreset{}, ErrPolicyDenied
}

func validateOrchestrationVersion(version HarnessVersion) (string, error) {
	selected := version.Orchestration
	preset, err := orchestrationPreset(version.Policy, selected.Preset)
	if err != nil {
		return "", err
	}
	if selected.Prepared == nil || selected.Prepared.Digest() != preset.Prepared.Digest() {
		return "", ErrPolicyDenied
	}
	for _, approved := range version.Policy.OrchestrationPresets {
		view, inspectErr := approved.Prepared.Inspect()
		if inspectErr != nil {
			return "", inspectErr
		}
		if view.Config.Scope.Canonical() != version.Scope.Canonical() {
			return "", ErrPolicyDenied
		}
		owner := false
		for _, participant := range view.Participants {
			child, childErr := participant.Inspect()
			if childErr != nil {
				return "", childErr
			}
			owner = owner || child.AgentID == version.AgentID
		}
		if !owner {
			return "", ErrPolicyDenied
		}
	}
	return selected.Prepared.Digest(), nil
}

func selectOrchestrationProposal(base HarnessVersion, proposal Proposal) (HarnessVersion, error) {
	if len(proposal.Edits) != 1 {
		return HarnessVersion{}, ErrInvalidSpec
	}
	edit := proposal.Edits[0]
	if edit.Kind != EditOrchestrationPreset || edit.Text != "" || edit.Tools != nil || edit.Number != nil {
		return HarnessVersion{}, ErrPolicyDenied
	}
	preset, err := orchestrationPreset(base.Policy, edit.Target)
	if err != nil {
		return HarnessVersion{}, err
	}
	base.Orchestration = &OrchestrationSelection{Preset: preset.Name, Prepared: preset.Prepared}
	return base, nil
}
