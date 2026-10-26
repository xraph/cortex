package cognitive

import (
	"errors"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/xraph/cortex/evidence"
)

// ErrInvalidContext rejects incomplete, malformed or unbounded selection inputs.
var ErrInvalidContext = errors.New("cortex: invalid context selection")

// ErrContextBudgetExceeded requires retrieval, a larger context or escalation.
// No partial selection is returned when required context cannot fit.
var ErrContextBudgetExceeded = errors.New("cortex: required context exceeds budget")

// ContextKind describes an item's purpose. Classification comes from the host.
type ContextKind string

// Task requirements are always pinned. Other items are pinned unless the host
// explicitly makes them optional; Cortex does not infer importance from text.
const (
	ContextObjective   ContextKind = "objective"
	ContextConstraint  ContextKind = "constraint"
	ContextAcceptance  ContextKind = "acceptance"
	ContextPlan        ContextKind = "plan"
	ContextBlocker     ContextKind = "blocker"
	ContextHistory     ContextKind = "history"
	ContextObservation ContextKind = "observation"
	ContextSummary     ContextKind = "summary"
)

// ContextOrigin records who supplied material. It is not a model message role.
type ContextOrigin string

// Host classifications must come from trusted application code. Never decode
// host authority or optional flags directly from model or retrieved content.
const (
	ContextHost         ContextOrigin = "host"
	ContextConversation ContextOrigin = "conversation"
	ContextRetrieved    ContextOrigin = "retrieved"
	ContextGenerated    ContextOrigin = "generated"
)

// ContextSource identifies immutable source bytes without exposing their content.
// A digest proves identity, not truth, authorization, freshness or availability.
type ContextSource struct {
	Reference string `json:"reference"`
	Digest    string `json:"digest"`
}

// ContextItem describes one indivisible unit, such as a complete conversation
// turn. The host must group dependent messages together and supply measured costs.
// Sources and Uncertainty preserve a summary's references and qualifications;
// this selector neither produces summaries nor verifies their semantic fidelity.
type ContextItem struct {
	Key         string          `json:"key"`
	Kind        ContextKind     `json:"kind"`
	Origin      ContextOrigin   `json:"origin"`
	Source      ContextSource   `json:"source"`
	Sources     []ContextSource `json:"sources,omitempty"`
	Uncertainty string          `json:"uncertainty,omitempty"`
	Tokens      int             `json:"tokens"`
	Optional    bool            `json:"optional,omitempty"`
}

// ContextPolicy reserves room for output and selects within a finite window.
// Revisions identify the trusted classification and token-counting implementations.
// Costs are supplied by the host. Before provider dispatch, the caller must count
// the complete assembled request to check framing and nonadditive tokenization.
type ContextPolicy struct {
	Summary         HistorySummaryPolicy `json:"summary,omitzero"`
	Revision        string               `json:"revision"`
	CounterRevision string               `json:"counter_revision"`
	MaxTokens       int                  `json:"max_tokens"`
	ReservedTokens  int                  `json:"reserved_tokens"`
}

// ContextInput lists atomic items oldest first. FixedTokens accounts for system
// instructions, tools and request framing that are never eligible for omission.
type ContextInput struct {
	FixedTokens int           `json:"fixed_tokens"`
	Items       []ContextItem `json:"items"`
}

// ContextSelection preserves order and source metadata in detached slices.
// RemainingTokens excludes reserved output tokens. Omitted items are an audit
// record for the host; do not append them to the model request without recounting.
type ContextSelection struct {
	PolicyRevision  string        `json:"policy_revision"`
	CounterRevision string        `json:"counter_revision"`
	Retained        []ContextItem `json:"retained"`
	Omitted         []ContextItem `json:"omitted"`
	InputTokens     int           `json:"input_tokens"`
	RemainingTokens int           `json:"remaining_tokens"`
}

// Select retains every required item, then considers optional items newest first.
// Selection never rewrites content, merges conflicting observations, elevates an
// origin or removes source references. The caller owns scoped source retrieval,
// publication screening, persistence and model-message construction.
func (p ContextPolicy) Select(in ContextInput) (ContextSelection, error) {
	if p.Summary.Validate() != nil {
		return ContextSelection{}, ErrInvalidContext
	}
	if !contextText(p.Revision, 256) || !contextText(p.CounterRevision, 256) || p.MaxTokens <= 0 || p.ReservedTokens < 0 || p.ReservedTokens > p.MaxTokens || in.FixedTokens < 0 || len(in.Items) > 4096 {
		return ContextSelection{}, ErrInvalidContext
	}
	seen := make(map[string]bool, len(in.Items))
	metadataBytes := 0
	for _, item := range in.Items {
		if !validContextItem(item, &metadataBytes) || seen[item.Key] {
			return ContextSelection{}, ErrInvalidContext
		}
		seen[item.Key] = true
	}
	remainingTokens := p.MaxTokens - p.ReservedTokens
	if in.FixedTokens > remainingTokens {
		return ContextSelection{}, ErrContextBudgetExceeded
	}
	remainingTokens -= in.FixedTokens
	kept := make([]bool, len(in.Items))
	for i, item := range in.Items {
		if item.Optional {
			continue
		}
		if item.Tokens > remainingTokens {
			return ContextSelection{}, ErrContextBudgetExceeded
		}
		remainingTokens -= item.Tokens
		kept[i] = true
	}
	for i := len(in.Items) - 1; i >= 0; i-- {
		item := in.Items[i]
		if item.Optional && item.Tokens <= remainingTokens {
			remainingTokens -= item.Tokens
			kept[i] = true
		}
	}
	result := ContextSelection{
		PolicyRevision: p.Revision, CounterRevision: p.CounterRevision,
		InputTokens: p.MaxTokens - p.ReservedTokens - remainingTokens, RemainingTokens: remainingTokens,
		Retained: make([]ContextItem, 0, len(in.Items)), Omitted: make([]ContextItem, 0),
	}
	for i, item := range in.Items {
		item.Sources = slices.Clone(item.Sources)
		if kept[i] {
			result.Retained = append(result.Retained, item)
		} else {
			result.Omitted = append(result.Omitted, item)
		}
	}
	return result, nil
}

func validContextItem(item ContextItem, metadataBytes *int) bool {
	if !contextText(item.Key, 256) || item.Tokens <= 0 || len(item.Sources) > 64 || !utf8.ValidString(item.Uncertainty) || len(item.Uncertainty) > 4096 {
		return false
	}
	switch item.Origin {
	case ContextHost, ContextConversation, ContextRetrieved, ContextGenerated:
	default:
		return false
	}
	switch item.Kind {
	case ContextObjective, ContextConstraint, ContextAcceptance, ContextPlan, ContextBlocker:
		if item.Origin != ContextHost || item.Optional {
			return false
		}
	case ContextSummary:
		if len(item.Sources) == 0 || strings.TrimSpace(item.Uncertainty) == "" {
			return false
		}
	case ContextHistory, ContextObservation:
	default:
		return false
	}
	*metadataBytes += len(item.Key) + len(item.Uncertainty)
	if !validContextSource(item.Source, metadataBytes) {
		return false
	}
	for _, source := range item.Sources {
		if !validContextSource(source, metadataBytes) {
			return false
		}
	}
	return true
}

func validContextSource(source ContextSource, metadataBytes *int) bool {
	if !contextText(source.Reference, 1024) || !evidence.ValidDigest(source.Digest) {
		return false
	}
	*metadataBytes += len(source.Reference) + len(source.Digest)
	return *metadataBytes <= 1<<20
}

func contextText(value string, limit int) bool {
	return len(value) <= limit && utf8.ValidString(value) && strings.TrimSpace(value) != ""
}
