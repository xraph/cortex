//go:build integration

package fabriqbrain

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"github.com/xraph/fabriq"
	"github.com/xraph/fabriq/adapters/postgres"
	"github.com/xraph/fabriq/core/agent"
	"github.com/xraph/fabriq/core/registry"
	"github.com/xraph/fabriq/core/tenant"
	"github.com/xraph/fabriq/fabriqtest"
	"github.com/xraph/fabriq/migrations"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/knowledge"
)

// hashEmbedder is a deterministic 768-dim embedder: equal text → equal vector,
// so a query equal to a stored row's content is its own nearest neighbour.
type hashEmbedder struct{}

func (hashEmbedder) Dims() int { return 768 }
func (hashEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		vec := make([]float32, 768)
		h := sha256.Sum256([]byte(t))
		for j := 0; j < 768; j++ {
			vec[j] = float32(h[j%32]) / 255.0
		}
		out[i] = vec
	}
	return out, nil
}

func TestE2E_LearningLoop_WriteVectorizeRecall(t *testing.T) {
	ctx := context.Background()
	const entity = "agent_memory"

	superDSN := fabriqtest.StartPostgres(t)

	// Registry with the memory entity.
	reg := registry.New()
	reg.MustRegister(MemorySpec(entity))
	if err := reg.Validate(); err != nil {
		t.Fatalf("registry validate: %v", err)
	}

	// Migrate fabriq's internal schema (creates fabriq_embeddings/pgvector).
	orch, closeFn, err := migrations.OpenOrchestrator(ctx, superDSN)
	if err != nil {
		t.Fatalf("OpenOrchestrator: %v", err)
	}
	if _, err := orch.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_ = closeFn()

	// Create the dynamic table as the schema owner BEFORE the app role exists,
	// so DEFAULT PRIVILEGES cover it (mirrors fabriq's dynamic E2E tests).
	owner, err := postgres.Open(ctx, superDSN, reg)
	if err != nil {
		t.Fatalf("postgres.Open (owner): %v", err)
	}
	ent, ok := reg.Get(entity)
	if !ok {
		t.Fatalf("entity %q not registered", entity)
	}
	if err := owner.EnsureDynamic(ctx, ent); err != nil {
		t.Fatalf("EnsureDynamic: %v", err)
	}
	_ = owner.Close()

	// Open fabriq with the app role (Postgres only — no Redis needed).
	appDSN := fabriqtest.CreateAppRole(t, superDSN)
	f, _, err := fabriq.Open(ctx, reg, fabriq.Config{
		Postgres: fabriq.PostgresConfig{DSN: appDSN},
	})
	if err != nil {
		t.Fatalf("fabriq.Open: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	tctx, err := tenant.WithTenant(ctx, "acme")
	if err != nil {
		t.Fatalf("WithTenant: %v", err)
	}

	emb := hashEmbedder{}

	// Build a write-enabled toolkit and the learning-loop plugin over the real fabric.
	tk, err := agent.NewToolkit(f, f.Registry(), emb, agent.Config{
		VectorDims: 768,
		Write:      MemoryWritePolicy(entity),
	})
	if err != nil {
		t.Fatalf("NewToolkit: %v", err)
	}
	plugin := NewPlugin(tk, WithMemoryEntity(entity))

	// Simulate an agent run: started → completed. The plugin writes the memory row.
	runID := id.NewAgentRunID()
	agentID := id.NewAgentID()
	if err := plugin.OnRunStarted(tctx, agentID, runID, "the coolant pump failed at station seven"); err != nil {
		t.Fatalf("OnRunStarted: %v", err)
	}
	if err := plugin.OnRunCompleted(tctx, agentID, runID, "replace the pump seal and restart", 2*time.Second); err != nil {
		t.Fatalf("OnRunCompleted: %v", err)
	}

	// Vectorize the written row(s) directly (no Redis embed worker needed).
	ix, err := agent.NewIndexer(f, f.Registry(), emb)
	if err != nil {
		t.Fatalf("NewIndexer: %v", err)
	}
	n, err := ix.Reindex(tctx, entity)
	if err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if n == 0 {
		t.Fatalf("Reindex indexed 0 rows; expected the memory row")
	}

	// Recall through the knowledge provider. The query text equals the stored
	// content so the hash vectors match: the memory must surface.
	provider := NewProvider(tk, WithEntities(entity))
	chunks, err := provider.Retrieve(tctx,
		"the coolant pump failed at station seven\nreplace the pump seal and restart",
		&knowledge.RetrieveParams{TopK: 5})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatalf("recall returned 0 chunks; the learning loop did not surface the memory")
	}
	top := chunks[0]
	if !strings.Contains(top.Content, "replace the pump seal") {
		t.Fatalf("top recalled chunk content = %q, want it to contain the run output", top.Content)
	}
}
