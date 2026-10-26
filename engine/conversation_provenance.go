package engine

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/memory"
	"github.com/xraph/cortex/run"
)

const (
	conversationSourceKey  = "_cortex_source_run"
	maxConversationWrites  = 512
	maxConversationSources = 128
)

// conversationMessageDigest excludes only the lookup hint. Source claims are
// authenticated against the scoped run's retained write receipt, never metadata.
func conversationMessageDigest(message memory.Message) (string, error) {
	metadata := make(map[string]any, len(message.Metadata))
	for key, value := range message.Metadata {
		if key != conversationSourceKey {
			metadata[key] = value
		}
	}
	message.Metadata = metadata
	data, err := json.Marshal(message)
	if err != nil {
		return "", errKnowledgeProvenance
	}
	return evidence.Digest(data), nil
}

func (e *Engine) recordConversationSources(ctx context.Context, st *reactState, messages []memory.Message) error {
	if len(messages) == 0 {
		return nil
	}
	publication, ok := ctx.Value(publicationContextKey{}).(publicationContext)
	if !ok || publication.run == nil || e.store == nil || publication.run.SessionID != st.sessionID || cortex.ScopeFromContext(ctx).Canonical() != publication.run.Scope.Canonical() {
		return errKnowledgeProvenance
	}
	current := publication.run
	next := *current
	next.Provenance.ConversationWrites = slices.Clone(current.Provenance.ConversationWrites)
	for index := range messages {
		digest, err := conversationMessageDigest(messages[index])
		if err != nil {
			return err
		}
		receipt := run.Revision{Name: "conversation-message", Digest: digest}
		if !slices.Contains(next.Provenance.ConversationWrites, receipt) {
			if len(next.Provenance.ConversationWrites) >= maxConversationWrites {
				return errKnowledgeProvenance
			}
			next.Provenance.ConversationWrites = append(next.Provenance.ConversationWrites, receipt)
		}
		if messages[index].Metadata == nil {
			messages[index].Metadata = make(map[string]any)
		}
		messages[index].Metadata[conversationSourceKey] = current.ID.String()
	}
	if err := e.store.UpdateRun(ctx, &next); err != nil {
		return errKnowledgeProvenance
	}
	*current = next
	st.provenance = current.Provenance
	return nil
}

// inheritConversationSources keeps execution usable when old history has no
// authenticated origin. That uncertainty survives future runs and bars learning.
func (e *Engine) inheritConversationSources(ctx context.Context, current *run.Run, messages []memory.Message) {
	sources := make(map[string]*run.Run)
	for _, message := range messages {
		sourceID, ok := message.Metadata[conversationSourceKey].(string)
		parsed, err := id.ParseAgentRunID(sourceID)
		if !ok || err != nil || parsed.IsNil() || parsed == current.ID {
			current.Provenance.UnattributedHistory = true
			continue
		}
		source, cached := sources[sourceID]
		if !cached {
			if len(sources) >= maxConversationSources {
				current.Provenance.UnattributedHistory = true
				continue
			}
			source, err = e.store.GetRun(ctx, parsed)
			if err != nil || source == nil || source.ID != parsed || source.Scope.Canonical() != current.Scope.Canonical() || source.AgentID != current.AgentID || source.SessionID != current.SessionID {
				source = nil
			}
			sources[sourceID] = source
		}
		if source == nil {
			current.Provenance.UnattributedHistory = true
			continue
		}
		digest, digestErr := conversationMessageDigest(message)
		if digestErr != nil || len(source.Provenance.ConversationWrites) > maxConversationWrites || !slices.Contains(source.Provenance.ConversationWrites, run.Revision{Name: "conversation-message", Digest: digest}) {
			current.Provenance.UnattributedHistory = true
			continue
		}
		refs, mergeErr := mergeRecalledLessons(current.Provenance.RecalledLessons, source.Provenance.RecalledLessons)
		if mergeErr != nil {
			current.Provenance.UnattributedHistory = true
			continue
		}
		current.Provenance.RecalledLessons = refs
		current.Provenance.UnattributedHistory = current.Provenance.UnattributedHistory || source.Provenance.UnattributedHistory
		for _, refs := range [][]run.Revision{source.Provenance.Sources, source.Provenance.SourceReads} {
			for _, ref := range refs {
				if strings.HasPrefix(ref.Name, "lesson:") {
					current.Provenance.UnattributedHistory = true
				}
			}
		}
	}
}
