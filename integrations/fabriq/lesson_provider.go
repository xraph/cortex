package fabriqbrain

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/knowledge"
	"github.com/xraph/cortex/learning"
)

// LessonProvider exposes governed lessons through the installed engine knowledge
// interface. Applicability and contraindications remain alongside each statement.
type LessonProvider struct {
	adapter *LessonAdapter
	domain  string
}

var _ knowledge.Provider = (*LessonProvider)(nil)

// NewLessonProvider binds one governed domain; query parameters cannot broaden it.
func NewLessonProvider(adapter *LessonAdapter, domain string) (*LessonProvider, error) {
	if adapter == nil || learning.ValidateContent(domain, "pending", nil, nil) != nil {
		return nil, learning.ErrInvalid
	}
	return &LessonProvider{adapter: adapter, domain: domain}, nil
}

// Retrieve returns individually qualified lessons and their evidence references.
func (p *LessonProvider) Retrieve(ctx context.Context, query string, params *knowledge.RetrieveParams) ([]knowledge.ScoredChunk, error) {
	limit := 5
	if params != nil {
		if params.Collection != "" && params.Collection != p.adapter.config.Entity || params.MinScore != 0 {
			return nil, learning.ErrInvalid
		}
		if params.TopK != 0 {
			limit = params.TopK
		}
	}
	page, err := p.adapter.Recall(ctx, learning.Query{Scope: cortex.ScopeFromContext(ctx), Domain: p.domain, HarnessSchema: 1, Query: query, Cutoff: p.adapter.config.Now().UTC(), Limit: limit})
	if err != nil {
		return nil, err
	}
	result := make([]knowledge.ScoredChunk, 0, len(page.Items))
	for _, lesson := range page.Items {
		body, encodeErr := json.Marshal(struct {
			Statement         string               `json:"statement"`
			Applicability     []string             `json:"applicability"`
			Contraindications []string             `json:"contraindications"`
			Reports           []learning.ReportRef `json:"source_reports"`
		}{lesson.Statement, lesson.Applicability, lesson.Contraindications, lesson.SourceReports})
		if encodeErr != nil {
			return nil, encodeErr
		}
		digest, digestErr := learning.Digest(lesson)
		if digestErr != nil {
			return nil, digestErr
		}
		result = append(result, knowledge.ScoredChunk{Content: string(body), Source: "lesson:" + lesson.ID.String(), DocumentID: lesson.ID.String(), CollectionID: p.adapter.config.Entity, Metadata: map[string]string{"evidence_class": string(lesson.EvidenceClass), "domain": lesson.Domain, "revision": strconv.FormatInt(lesson.Revision, 10), "digest": digest}})
	}
	return result, ctx.Err()
}

// ListCollections checks the same current read grant used by retrieval.
func (p *LessonProvider) ListCollections(ctx context.Context) ([]knowledge.CollectionInfo, error) {
	if err := p.adapter.config.Guard.Authorize(ctx, learning.ActionRead, id.Nil, p.domain); err != nil {
		return nil, err
	}
	return []knowledge.CollectionInfo{{ID: p.adapter.config.Entity, Name: p.domain}}, nil
}
