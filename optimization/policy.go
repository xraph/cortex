package optimization

import (
	"bytes"
	"fmt"
	"slices"
	"strings"

	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization/internal/canonical"
)

// EditPolicy is a frozen host allowlist. Bounds must be positive and finite.
type EditPolicy struct {
	MutableSections         []string `json:"mutable_sections"`
	MutableToolDescriptions []string `json:"mutable_tool_descriptions"`
	AllowToolSubset         bool     `json:"allow_tool_subset"`
	TemperatureMin          *float64 `json:"temperature_min,omitempty"`
	TemperatureMax          *float64 `json:"temperature_max,omitempty"`
	MaxBodyBytes            int      `json:"max_body_bytes"`
	MaxPatchBytes           int      `json:"max_patch_bytes"`
	MaxEdits                int      `json:"max_edits"`
}

// EditKind enumerates the only fields a candidate may propose changing.
type EditKind string

const (
	EditSectionBody     EditKind = "section_body"
	EditToolDescription EditKind = "tool_description"
	EditToolSubset      EditKind = "tool_subset"
	EditTemperature     EditKind = "temperature"
)

// Edit carries one typed change. A non-nil empty Tools list means deny all tools.
type Edit struct {
	Kind   EditKind  `json:"kind"`
	Target string    `json:"target,omitempty"`
	Text   string    `json:"text,omitempty"`
	Tools  *[]string `json:"tools,omitempty"`
	Number *float64  `json:"number,omitempty"`
}

// Proposal contains candidate edits and their explanation, never host settings.
type Proposal struct {
	Edits       []Edit  `json:"edits"`
	Rationale   string  `json:"rationale,omitempty"`
	EvidenceIDs []id.ID `json:"evidence_ids,omitempty"`
}

// DecodeProposal rejects unknown and duplicate JSON members at every depth.
func DecodeProposal(data []byte) (Proposal, error) {
	var proposal Proposal
	if len(data) > 1<<20 {
		return proposal, fmt.Errorf("%w: oversized proposal", ErrInvalidSpec)
	}
	if err := canonical.Decode(data, &proposal); err != nil {
		return Proposal{}, fmt.Errorf("%w: %w", ErrInvalidSpec, err)
	}
	return proposal, nil
}

func (policy EditPolicy) validate() error {
	if policy.MaxBodyBytes <= 0 || policy.MaxPatchBytes <= 0 || policy.MaxEdits <= 0 {
		return fmt.Errorf("%w: edit bounds must be positive", ErrInvalidSpec)
	}
	if err := uniqueNames(policy.MutableSections); err != nil {
		return err
	}
	if err := uniqueNames(policy.MutableToolDescriptions); err != nil {
		return err
	}
	if (policy.TemperatureMin == nil) != (policy.TemperatureMax == nil) {
		return fmt.Errorf("%w: sampling bounds require both endpoints", ErrInvalidSpec)
	}
	if policy.TemperatureMin != nil {
		low, high := *policy.TemperatureMin, *policy.TemperatureMax
		if !finite(low) || !finite(high) || low < 0 || high > 2 || low > high {
			return fmt.Errorf("%w: invalid sampling bounds", ErrInvalidSpec)
		}
	}
	return nil
}

func uniqueNames(names []string) error {
	seen := map[string]bool{}
	for _, name := range names {
		if strings.TrimSpace(name) == "" || strings.TrimSpace(name) != name || seen[name] {
			return fmt.Errorf("%w: empty or duplicate name", ErrInvalidSpec)
		}
		seen[name] = true
	}
	return nil
}

// ValidateProposal derives a detached version through the root engine's frozen
// edit API. It refuses a changed host policy and never reloads live sources.
func ValidateProposal(base HarnessVersion, proposal Proposal, policy EditPolicy) (HarnessVersion, error) {
	sealed, err := SealVersion(base)
	if err != nil {
		return HarnessVersion{}, err
	}
	if base.Digest == "" || sealed.Digest != base.Digest {
		return HarnessVersion{}, fmt.Errorf("%w: baseline digest changed", ErrStaleVersion)
	}
	if validationErr := policy.validate(); validationErr != nil {
		return HarnessVersion{}, validationErr
	}
	before, err := canonical.Marshal(base.Policy)
	if err != nil {
		return HarnessVersion{}, err
	}
	current, err := canonical.Marshal(policy)
	if err != nil {
		return HarnessVersion{}, err
	}
	if !bytes.Equal(before, current) {
		return HarnessVersion{}, ErrPolicyDenied
	}
	if base.Kind != HarnessExecution {
		return HarnessVersion{}, fmt.Errorf("%w: execution proposal requires execution harness", ErrInvalidSpec)
	}
	if len(proposal.Edits) == 0 || len(proposal.Edits) > policy.MaxEdits {
		return HarnessVersion{}, fmt.Errorf("%w: invalid edit count", ErrInvalidSpec)
	}
	patch, err := canonical.Marshal(proposal.Edits)
	if err != nil {
		return HarnessVersion{}, fmt.Errorf("%w: %w", ErrInvalidSpec, err)
	}
	if len(patch) > policy.MaxPatchBytes {
		return HarnessVersion{}, fmt.Errorf("%w: patch too large", ErrPolicyDenied)
	}
	edits := engine.PreparedEdits{SectionBodies: map[string]string{}, ToolDescriptions: map[string]string{}}
	seen := map[string]bool{}
	for _, edit := range proposal.Edits {
		key := string(edit.Kind) + "/" + edit.Target
		if seen[key] {
			return HarnessVersion{}, fmt.Errorf("%w: duplicate edit target", ErrInvalidSpec)
		}
		seen[key] = true
		switch edit.Kind {
		case EditSectionBody, EditToolDescription:
			if edit.Target == "" || edit.Tools != nil || edit.Number != nil {
				return HarnessVersion{}, fmt.Errorf("%w: invalid text edit", ErrInvalidSpec)
			}
			if len(edit.Text) > policy.MaxBodyBytes {
				return HarnessVersion{}, ErrPolicyDenied
			}
			if edit.Kind == EditSectionBody {
				if !slices.Contains(policy.MutableSections, edit.Target) {
					return HarnessVersion{}, ErrPolicyDenied
				}
				edits.SectionBodies[edit.Target] = edit.Text
			} else {
				if !slices.Contains(policy.MutableToolDescriptions, edit.Target) {
					return HarnessVersion{}, ErrPolicyDenied
				}
				edits.ToolDescriptions[edit.Target] = edit.Text
			}
		case EditToolSubset:
			if edit.Target != "" || edit.Text != "" || edit.Number != nil || edit.Tools == nil {
				return HarnessVersion{}, fmt.Errorf("%w: invalid tool subset edit", ErrInvalidSpec)
			}
			if !policy.AllowToolSubset {
				return HarnessVersion{}, ErrPolicyDenied
			}
			if validationErr := uniqueNames(*edit.Tools); validationErr != nil {
				return HarnessVersion{}, validationErr
			}
			edits.Tools = edit.Tools
		case EditTemperature:
			if edit.Target != "" || edit.Text != "" || edit.Tools != nil || edit.Number == nil || !finite(*edit.Number) {
				return HarnessVersion{}, fmt.Errorf("%w: invalid sampling edit", ErrInvalidSpec)
			}
			if policy.TemperatureMin == nil || *edit.Number < *policy.TemperatureMin || *edit.Number > *policy.TemperatureMax {
				return HarnessVersion{}, ErrPolicyDenied
			}
			edits.Temperature = edit.Number
		default:
			return HarnessVersion{}, fmt.Errorf("%w: unsupported edit kind", ErrInvalidSpec)
		}
	}
	prepared, err := base.Prepared.Derive(edits)
	if err != nil {
		return HarnessVersion{}, fmt.Errorf("%w: %w", ErrPolicyDenied, err)
	}
	sealed.ID = id.New(PrefixVersion)
	sealed.ParentID = base.ID
	sealed.Prepared = prepared
	sealed.Digest = ""
	return SealVersion(sealed)
}
