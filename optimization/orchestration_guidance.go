package optimization

import (
	"fmt"
	"slices"

	"github.com/xraph/cortex/engine"
)

func deriveOrchestrationProposal(base HarnessVersion, proposal Proposal) (HarnessVersion, error) {
	selected := *base.Orchestration
	switched := false
	for _, edit := range proposal.Edits {
		if edit.Kind != EditOrchestrationPreset {
			continue
		}
		if switched || edit.Participant != "" || edit.Text != "" || edit.Tools != nil || edit.Number != nil {
			return HarnessVersion{}, ErrPolicyDenied
		}
		preset, err := orchestrationPreset(base.Policy, edit.Target)
		if err != nil {
			return HarnessVersion{}, err
		}
		selected = OrchestrationSelection{Preset: preset.Name, Prepared: preset.Prepared}
		switched = true
	}
	preset, err := orchestrationPreset(base.Policy, selected.Preset)
	if err != nil {
		return HarnessVersion{}, err
	}
	seen := make(map[[2]string]bool)
	for _, edit := range proposal.Edits {
		if edit.Kind == EditOrchestrationPreset {
			continue
		}
		target := [2]string{edit.Participant, edit.Target}
		if edit.Kind != EditParticipantGuidance || edit.Tools != nil || edit.Number != nil || seen[target] || len(edit.Text) > base.Policy.MaxBodyBytes {
			return HarnessVersion{}, ErrPolicyDenied
		}
		seen[target] = true
		original, bodyErr := presetGuidanceBody(preset, edit.Participant, edit.Target)
		if bodyErr != nil {
			return HarnessVersion{}, bodyErr
		}
		if edit.Text == original {
			delete(selected.Guidance[edit.Participant], edit.Target)
			if len(selected.Guidance[edit.Participant]) == 0 {
				delete(selected.Guidance, edit.Participant)
			}
			continue
		}
		if selected.Guidance == nil {
			selected.Guidance = make(map[string]map[string]string)
		}
		if selected.Guidance[edit.Participant] == nil {
			selected.Guidance[edit.Participant] = make(map[string]string)
		}
		selected.Guidance[edit.Participant][edit.Target] = edit.Text
	}
	selected.Prepared, err = derivePresetGuidance(preset, selected.Guidance, base.Policy.MaxBodyBytes)
	if err != nil {
		return HarnessVersion{}, err
	}
	base.Orchestration = &selected
	return base, nil
}

// Every derivation starts at the approved snapshot, so repeated edits preserve
// provenance and resetting a section restores the original content identity.
func derivePresetGuidance(preset OrchestrationPreset, guidance map[string]map[string]string, maxBodyBytes int) (*engine.PreparedOrchestration, error) {
	if len(guidance) == 0 {
		return preset.Prepared, nil
	}
	edits := make(map[string]engine.PreparedEdits, len(guidance))
	for participant, sections := range guidance {
		if len(sections) == 0 {
			return nil, ErrInvalidSpec
		}
		for section, body := range sections {
			original, err := presetGuidanceBody(preset, participant, section)
			if err != nil {
				return nil, err
			}
			if body == original || len(body) > maxBodyBytes {
				return nil, ErrPolicyDenied
			}
		}
		edits[participant] = engine.PreparedEdits{SectionBodies: sections}
	}
	derived, err := preset.Prepared.Derive(edits)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPolicyDenied, err)
	}
	return derived, nil
}

func presetGuidanceBody(preset OrchestrationPreset, participant, section string) (string, error) {
	if !slices.Contains(preset.Guidance[participant], section) {
		return "", ErrPolicyDenied
	}
	view, err := preset.Prepared.Inspect()
	if err != nil {
		return "", err
	}
	child := view.Participants[participant]
	if child == nil {
		return "", ErrPolicyDenied
	}
	fields, err := child.Inspect()
	if err != nil {
		return "", err
	}
	found, body := 0, ""
	for _, part := range fields.Sections {
		if part.ID == section && !part.Locked {
			found++
			body = part.Body
		}
	}
	if found != 1 {
		return "", ErrPolicyDenied
	}
	return body, nil
}
