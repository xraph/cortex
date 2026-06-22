package fabriqbrain

import (
	"context"
	"strconv"
	"strings"

	"github.com/xraph/cortex/knowledge"
	"github.com/xraph/fabriq/core/agent"
)

// recaller is the narrow slice of *agent.Toolkit the provider needs.
type recaller interface {
	Recall(ctx context.Context, req agent.RecallRequest) (agent.ContextPack, error)
}

// Adapter implements cortex's knowledge.Provider over fabriq's recall pipeline.
type Adapter struct {
	rec recaller
	cfg config
}

var _ knowledge.Provider = (*Adapter)(nil)

// NewProvider builds a knowledge.Provider over a fabriq recaller (a
// *agent.Toolkit satisfies recaller).
func NewProvider(rec recaller, opts ...Option) *Adapter {
	c := applyOptions(opts)
	if c.render == nil {
		c.render = defaultRender
	}
	return &Adapter{rec: rec, cfg: c}
}

// Retrieve runs fabriq recall and maps the context pack to scored chunks.
func (a *Adapter) Retrieve(ctx context.Context, query string, p *knowledge.RetrieveParams) ([]knowledge.ScoredChunk, error) {
	ctx = a.cfg.tenant(ctx)

	entities := a.cfg.entities
	topK := 0
	minScore := 0.0
	if p != nil {
		if p.Collection != "" {
			entities = []string{p.Collection}
		}
		topK = p.TopK
		minScore = p.MinScore
	}

	req := agent.RecallRequest{
		Query:    query,
		Budget:   a.cfg.budget,
		Entities: entities,
		K:        topK,
	}
	pack, err := a.rec.Recall(ctx, req)
	if err != nil {
		return nil, err
	}

	out := make([]knowledge.ScoredChunk, 0, len(pack.Items))
	for _, it := range pack.Items {
		if minScore > 0 && it.Score < minScore {
			continue
		}
		out = append(out, knowledge.ScoredChunk{
			Content:      a.cfg.render(it),
			Score:        it.Score,
			Source:       strings.Join(it.Source, "+"),
			DocumentID:   it.ID,
			CollectionID: it.Entity,
			Metadata: map[string]string{
				"entity":   it.Entity,
				"channels": strings.Join(it.Source, "+"),
				"tokens":   strconv.Itoa(it.Tokens),
			},
		})
		if topK > 0 && len(out) >= topK {
			break
		}
	}
	return out, nil
}

// ListCollections reports the configured recall-able entities as collections.
// Counts are best-effort (0) — fabriq entities are not document collections.
func (a *Adapter) ListCollections(_ context.Context) ([]knowledge.CollectionInfo, error) {
	out := make([]knowledge.CollectionInfo, 0, len(a.cfg.entities))
	for _, e := range a.cfg.entities {
		out = append(out, knowledge.CollectionInfo{ID: e, Name: e})
	}
	return out, nil
}
