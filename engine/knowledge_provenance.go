package engine

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/knowledge"
	"github.com/xraph/cortex/run"
)

var errKnowledgeProvenance = errors.New("cortex: knowledge provenance unavailable")

const maxRecalledLessons = 256

// recordRecalledKnowledge retains provider identities before exposing content.
// Preparation freezes them with the prompt. Tool retrieval persists them before
// the model can consume the result, including on streaming and resumed runs.
func (e *Engine) recordRecalledKnowledge(ctx context.Context, chunks []knowledge.ScoredChunk) error {
	refs := make([]run.Revision, 0, len(chunks))
	for _, chunk := range chunks {
		if !strings.HasPrefix(chunk.Source, "lesson:") && !strings.HasPrefix(chunk.DocumentID, "lesson_") {
			continue
		}
		name := strings.TrimPrefix(chunk.Source, "lesson:")
		key, err := id.Parse(name)
		revision, revisionErr := strconv.ParseInt(chunk.Metadata["revision"], 10, 64)
		if !strings.HasPrefix(chunk.Source, "lesson:") || err != nil || key.IsNil() || key.Prefix() != "lesson" || chunk.DocumentID != name || revisionErr != nil || revision < 1 || strconv.FormatInt(revision, 10) != chunk.Metadata["revision"] || !evidence.ValidDigest(chunk.Metadata["digest"]) {
			return errKnowledgeProvenance
		}
		refs = append(refs, run.Revision{Name: chunk.Source, Revision: chunk.Metadata["revision"], Digest: chunk.Metadata["digest"]})
	}
	if len(refs) == 0 {
		return nil
	}
	if capture, ok := ctx.Value(sourceReadSetKey{}).(*sourceReadSet); ok && capture != nil {
		merged, err := mergeRecalledLessons(capture.recalledLessons, refs)
		if err != nil {
			return err
		}
		capture.recalledLessons = merged
		return nil
	}
	publication, ok := ctx.Value(publicationContextKey{}).(publicationContext)
	runtime, runtimeOK := ctx.Value(operationalContextKey{}).(operationalRuntime)
	if !ok || publication.run == nil || !runtimeOK || runtime.state == nil || e.store == nil || runtime.runID != publication.run.ID || publication.run.State != run.StateRunning || cortex.ScopeFromContext(ctx).Canonical() != publication.run.Scope.Canonical() {
		return errKnowledgeProvenance
	}
	current := publication.run
	merged, err := mergeRecalledLessons(current.Provenance.RecalledLessons, refs)
	if err != nil {
		return err
	}
	if slices.Equal(merged, current.Provenance.RecalledLessons) {
		return nil
	}
	next := *current
	next.Provenance.RecalledLessons = merged
	if err = e.store.UpdateRun(ctx, &next); err != nil {
		return errKnowledgeProvenance
	}
	*current = next
	runtime.state.provenance = current.Provenance
	return nil
}

func mergeRecalledLessons(current, refs []run.Revision) ([]run.Revision, error) {
	merged := slices.Clone(current)
	for _, ref := range refs {
		if slices.Contains(merged, ref) {
			continue
		}
		if len(merged) >= maxRecalledLessons {
			return nil, errKnowledgeProvenance
		}
		merged = append(merged, ref)
	}
	return merged, nil
}
