package cognitive_test

import (
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/cognitive"
	"github.com/xraph/cortex/evidence"
)

func contextItem(key string, kind cognitive.ContextKind, tokens int) cognitive.ContextItem {
	return cognitive.ContextItem{
		Key: key, Kind: kind, Origin: cognitive.ContextHost, Tokens: tokens,
		Source: cognitive.ContextSource{Reference: "fixture:" + key, Digest: evidence.Digest([]byte(key))},
	}
}

func contextPolicy() cognitive.ContextPolicy {
	return cognitive.ContextPolicy{Revision: "host-context/v1", CounterRevision: "fixture-counter/v1", MaxTokens: 100, ReservedTokens: 20}
}

func TestContextCompactionRetainsConstraintsAndSources(t *testing.T) {
	constraint := contextItem("constraint", cognitive.ContextConstraint, 20)
	old := contextItem("old", cognitive.ContextHistory, 25)
	old.Origin, old.Optional = cognitive.ContextConversation, true
	recent := contextItem("recent", cognitive.ContextHistory, 25)
	recent.Origin, recent.Optional = cognitive.ContextConversation, true
	objective := contextItem("objective", cognitive.ContextObjective, 20)
	selected, err := contextPolicy().Select(cognitive.ContextInput{FixedTokens: 10, Items: []cognitive.ContextItem{constraint, old, recent, objective}})
	require.NoError(t, err)
	require.Equal(t, []cognitive.ContextItem{constraint, recent, objective}, selected.Retained)
	require.Equal(t, []cognitive.ContextItem{old}, selected.Omitted)
	require.Equal(t, 75, selected.InputTokens)
	require.Equal(t, 5, selected.RemainingTokens)
	require.Equal(t, "host-context/v1", selected.PolicyRevision)
	require.Equal(t, "fixture-counter/v1", selected.CounterRevision)
	// Omission carries the original source identity; selection does not summarize it.
	require.Equal(t, "fixture:old", selected.Omitted[0].Source.Reference)
	require.Equal(t, old.Source.Digest, selected.Omitted[0].Source.Digest)
}

func TestContextRequiredOverflowReturnsNoPartialPrompt(t *testing.T) {
	for _, kind := range []cognitive.ContextKind{
		cognitive.ContextObjective, cognitive.ContextConstraint, cognitive.ContextAcceptance,
		cognitive.ContextPlan, cognitive.ContextBlocker, cognitive.ContextHistory,
	} {
		t.Run(string(kind), func(t *testing.T) {
			selected, err := contextPolicy().Select(cognitive.ContextInput{FixedTokens: 10, Items: []cognitive.ContextItem{contextItem("required", kind, 71)}})
			require.ErrorIs(t, err, cognitive.ErrContextBudgetExceeded)
			require.Zero(t, selected)
		})
	}
}

func TestContextCannotMarkTaskRequirementsOptional(t *testing.T) {
	for _, kind := range []cognitive.ContextKind{
		cognitive.ContextObjective, cognitive.ContextConstraint, cognitive.ContextAcceptance,
		cognitive.ContextPlan, cognitive.ContextBlocker,
	} {
		item := contextItem("required", kind, 10)
		item.Optional = true
		selected, err := contextPolicy().Select(cognitive.ContextInput{Items: []cognitive.ContextItem{item}})
		require.ErrorIs(t, err, cognitive.ErrInvalidContext)
		require.Zero(t, selected)
	}
}

func TestUntrustedMemoryCannotBecomePolicy(t *testing.T) {
	for _, origin := range []cognitive.ContextOrigin{cognitive.ContextConversation, cognitive.ContextRetrieved, cognitive.ContextGenerated} {
		item := contextItem("claimed-policy", cognitive.ContextConstraint, 10)
		item.Origin = origin
		selected, err := contextPolicy().Select(cognitive.ContextInput{Items: []cognitive.ContextItem{item}})
		require.ErrorIs(t, err, cognitive.ErrInvalidContext)
		require.Zero(t, selected)
	}
}

func TestContextSelectionPreservesContradictoryObservations(t *testing.T) {
	first := contextItem("source-a-says-open", cognitive.ContextObservation, 10)
	first.Origin = cognitive.ContextRetrieved
	second := contextItem("source-b-says-closed", cognitive.ContextObservation, 10)
	second.Origin = cognitive.ContextRetrieved
	in := cognitive.ContextInput{Items: []cognitive.ContextItem{first, second}}
	selected, err := contextPolicy().Select(in)
	require.NoError(t, err)
	require.Equal(t, in.Items, selected.Retained)
	require.Empty(t, selected.Omitted)
}

func TestContextSummaryRetainsSourcesAndUncertainty(t *testing.T) {
	item := contextItem("summary", cognitive.ContextSummary, 10)
	item.Origin = cognitive.ContextGenerated
	item.Sources = []cognitive.ContextSource{{Reference: "fixture:original", Digest: evidence.Digest([]byte("original"))}}
	item.Uncertainty = "Two sources disagree; confirmation pending."
	selected, err := contextPolicy().Select(cognitive.ContextInput{Items: []cognitive.ContextItem{item}})
	require.NoError(t, err)
	require.Equal(t, item, selected.Retained[0])
	item.Sources[0].Reference = "changed"
	require.Equal(t, "fixture:original", selected.Retained[0].Sources[0].Reference)
	require.Equal(t, cognitive.ContextGenerated, selected.Retained[0].Origin)
	require.Equal(t, "Two sources disagree; confirmation pending.", selected.Retained[0].Uncertainty)
}

func TestContextRejectsMalformedSelectionInputs(t *testing.T) {
	for name, change := range map[string]func(*cognitive.ContextPolicy, *cognitive.ContextInput){
		"missing policy revision":  func(p *cognitive.ContextPolicy, _ *cognitive.ContextInput) { p.Revision = "" },
		"missing counter revision": func(p *cognitive.ContextPolicy, _ *cognitive.ContextInput) { p.CounterRevision = "" },
		"unbounded budget":         func(p *cognitive.ContextPolicy, _ *cognitive.ContextInput) { p.MaxTokens = 0 },
		"negative reserve":         func(p *cognitive.ContextPolicy, _ *cognitive.ContextInput) { p.ReservedTokens = -1 },
		"reserve exceeds budget":   func(p *cognitive.ContextPolicy, _ *cognitive.ContextInput) { p.ReservedTokens = 101 },
		"negative fixed cost":      func(_ *cognitive.ContextPolicy, in *cognitive.ContextInput) { in.FixedTokens = -1 },
		"missing key":              func(_ *cognitive.ContextPolicy, in *cognitive.ContextInput) { in.Items[0].Key = "" },
		"duplicate key":            func(_ *cognitive.ContextPolicy, in *cognitive.ContextInput) { in.Items = append(in.Items, in.Items[0]) },
		"missing digest":           func(_ *cognitive.ContextPolicy, in *cognitive.ContextInput) { in.Items[0].Source.Digest = "" },
		"missing reference":        func(_ *cognitive.ContextPolicy, in *cognitive.ContextInput) { in.Items[0].Source.Reference = "" },
		"invalid text": func(_ *cognitive.ContextPolicy, in *cognitive.ContextInput) {
			in.Items[0].Uncertainty = string([]byte{0xff})
		},
		"unmeasured item": func(_ *cognitive.ContextPolicy, in *cognitive.ContextInput) { in.Items[0].Tokens = 0 },
		"negative tokens": func(_ *cognitive.ContextPolicy, in *cognitive.ContextInput) { in.Items[0].Tokens = -1 },
		"unknown kind":    func(_ *cognitive.ContextPolicy, in *cognitive.ContextInput) { in.Items[0].Kind = "unknown" },
		"unknown origin":  func(_ *cognitive.ContextPolicy, in *cognitive.ContextInput) { in.Items[0].Origin = "unknown" },
		"summary without sources": func(_ *cognitive.ContextPolicy, in *cognitive.ContextInput) {
			in.Items[0].Kind = cognitive.ContextSummary
		},
		"summary without uncertainty": func(_ *cognitive.ContextPolicy, in *cognitive.ContextInput) {
			in.Items[0].Kind = cognitive.ContextSummary
			in.Items[0].Sources = []cognitive.ContextSource{in.Items[0].Source}
		},
		"too many items": func(_ *cognitive.ContextPolicy, in *cognitive.ContextInput) {
			in.Items = make([]cognitive.ContextItem, 4097)
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := contextPolicy()
			in := cognitive.ContextInput{Items: []cognitive.ContextItem{contextItem("item", cognitive.ContextHistory, 10)}}
			change(&p, &in)
			selected, err := p.Select(in)
			require.ErrorIs(t, err, cognitive.ErrInvalidContext)
			require.Zero(t, selected)
		})
	}
}

func TestContextBudgetAccountingCannotOverflow(t *testing.T) {
	p := contextPolicy()
	p.MaxTokens, p.ReservedTokens = math.MaxInt, 1
	selected, err := p.Select(cognitive.ContextInput{FixedTokens: math.MaxInt - 2, Items: []cognitive.ContextItem{contextItem("required", cognitive.ContextHistory, 3)}})
	require.ErrorIs(t, err, cognitive.ErrContextBudgetExceeded)
	require.Zero(t, selected)
	selected, err = p.Select(cognitive.ContextInput{FixedTokens: math.MaxInt - 2, Items: []cognitive.ContextItem{contextItem("required", cognitive.ContextHistory, 1)}})
	require.NoError(t, err)
	require.Equal(t, math.MaxInt-1, selected.InputTokens)
	require.Zero(t, selected.RemainingTokens)
}

func TestContextSelectionValidatesEvenOmittedItems(t *testing.T) {
	item := contextItem("optional", cognitive.ContextHistory, 1000)
	item.Optional = true
	item.Source.Digest = "unverifiable"
	selected, err := contextPolicy().Select(cognitive.ContextInput{Items: []cognitive.ContextItem{item}})
	require.ErrorIs(t, err, cognitive.ErrInvalidContext)
	require.Zero(t, selected)
}

func TestContextOptionalGroupsAreNeverSplit(t *testing.T) {
	large := contextItem("complete-tool-turn", cognitive.ContextHistory, 71)
	large.Optional = true
	small := contextItem("earlier-turn", cognitive.ContextHistory, 5)
	small.Optional = true
	selected, err := contextPolicy().Select(cognitive.ContextInput{FixedTokens: 10, Items: []cognitive.ContextItem{small, large}})
	require.NoError(t, err)
	require.Equal(t, []cognitive.ContextItem{small}, selected.Retained)
	require.Equal(t, []cognitive.ContextItem{large}, selected.Omitted)
	require.Equal(t, 15, selected.InputTokens)
	selected, err = contextPolicy().Select(cognitive.ContextInput{FixedTokens: 81})
	require.ErrorIs(t, err, cognitive.ErrContextBudgetExceeded)
	require.Zero(t, selected)
}

func TestContextOmittedSummarySourcesAreDetached(t *testing.T) {
	item := contextItem("summary", cognitive.ContextSummary, 100)
	item.Optional = true
	item.Sources = []cognitive.ContextSource{{Reference: "fixture:original", Digest: evidence.Digest([]byte("original"))}}
	item.Uncertainty = "Unverified."
	selected, err := contextPolicy().Select(cognitive.ContextInput{Items: []cognitive.ContextItem{item}})
	require.NoError(t, err)
	require.Empty(t, selected.Retained)
	item.Sources[0].Reference = "changed"
	require.Equal(t, "fixture:original", selected.Omitted[0].Sources[0].Reference)
	selected.Omitted[0].Sources[0].Reference = "changed-again"
	require.Equal(t, "changed", item.Sources[0].Reference)
}

func TestContextSelectionBoundsMetadataEvenWhenEverythingIsOptional(t *testing.T) {
	items := make([]cognitive.ContextItem, 1024)
	for i := range items {
		items[i] = contextItem(strconv.Itoa(i), cognitive.ContextHistory, 1)
		items[i].Optional = true
		items[i].Uncertainty = strings.Repeat("x", 4096)
	}
	selected, err := contextPolicy().Select(cognitive.ContextInput{Items: items})
	require.ErrorIs(t, err, cognitive.ErrInvalidContext)
	require.Zero(t, selected)
}
