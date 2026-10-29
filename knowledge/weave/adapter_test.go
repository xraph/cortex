package weave_test

import (
	"context"
	"errors"
	"hash/fnv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/knowledge"
	weaveknowledge "github.com/xraph/cortex/knowledge/weave"

	"github.com/xraph/weave"
	"github.com/xraph/weave/chunker"
	"github.com/xraph/weave/collection"
	"github.com/xraph/weave/embedder"
	weaveengine "github.com/xraph/weave/engine"
	memstore "github.com/xraph/weave/store/memory"
	memvec "github.com/xraph/weave/vectorstore/memory"
)

const dims = 16

// fakeEmbedder maps each word to a bucket, so texts that share words are
// close and the result never depends on a network call.
type fakeEmbedder struct{}

func (fakeEmbedder) Dimensions() int { return dims }

func (fakeEmbedder) Embed(_ context.Context, texts []string) ([]embedder.EmbedResult, error) {
	out := make([]embedder.EmbedResult, len(texts))
	for i, text := range texts {
		v := make([]float32, dims)
		for _, w := range strings.Fields(strings.ToLower(text)) {
			h := fnv.New32a()
			_, _ = h.Write([]byte(w))
			v[h.Sum32()%dims]++
		}
		out[i] = embedder.EmbedResult{Vector: v, TokenCount: len(texts[i])}
	}
	return out, nil
}

type fixture struct {
	eng  *weaveengine.Engine
	cols map[string]string // "tenant/name" to collection ID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	eng, err := weaveengine.New(
		weaveengine.WithStore(memstore.New()),
		weaveengine.WithVectorStore(memvec.New()),
		weaveengine.WithEmbedder(fakeEmbedder{}),
		weaveengine.WithChunker(chunker.NewRecursiveChunker()),
	)
	require.NoError(t, err)
	f := &fixture{eng: eng, cols: map[string]string{}}
	f.ingest(t, "o1", "runbooks", "restart the payments worker after a deploy")
	f.ingest(t, "o2", "runbooks", "restart the secret worker of organization two")
	f.ingest(t, "docs", "loomplane-docs", "restart a worker from the loomplane dashboard")
	return f
}

func (f *fixture) ingest(t *testing.T, tenant, name, content string) {
	t.Helper()
	ctx := weave.WithTenant(context.Background(), tenant)
	col := &collection.Collection{Name: name, EmbeddingDims: dims}
	require.NoError(t, f.eng.CreateCollection(ctx, col))
	_, err := f.eng.Ingest(ctx, &weaveengine.IngestInput{CollectionID: col.ID, Title: name, Content: content})
	require.NoError(t, err)
	f.cols[tenant+"/"+name] = col.ID.String()
}

func scoped(org string) context.Context {
	return cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: org}}})
}

func contents(cs []knowledge.ScoredChunk) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Content
	}
	return out
}

func joined(cs []knowledge.ScoredChunk) string { return strings.Join(contents(cs), "\n") }

func TestRetrievePassesRunTenant(t *testing.T) {
	f := newFixture(t)
	a := weaveknowledge.New(f.eng)

	got, err := a.Retrieve(scoped("o1"), "restart the worker", &knowledge.RetrieveParams{TopK: 10})
	require.NoError(t, err)
	require.NotEmpty(t, got)
	require.Contains(t, joined(got), "payments")
	require.NotContains(t, joined(got), "secret")
	require.NotContains(t, joined(got), "dashboard")
	for _, c := range got {
		require.Equal(t, f.cols["o1/runbooks"], c.CollectionID)
	}
}

func TestRetrieveWithoutScopeFails(t *testing.T) {
	f := newFixture(t)
	a := weaveknowledge.New(f.eng)

	_, err := a.Retrieve(context.Background(), "restart", nil)
	require.True(t, errors.Is(err, cortex.ErrNoScope), "got %v", err)
	require.True(t, errors.Is(err, weave.ErrTenantRequired), "got %v", err)

	_, err = a.ListCollections(context.Background())
	require.True(t, errors.Is(err, cortex.ErrNoScope), "got %v", err)
	require.True(t, errors.Is(err, weave.ErrTenantRequired), "got %v", err)

	// A TenantFunc that names no tenant gets the same refusal.
	empty := weaveknowledge.New(f.eng, weaveknowledge.WithTenant(func(context.Context, cortex.Scope) (string, error) { return "", nil }))
	_, err = empty.Retrieve(scoped("o1"), "restart", nil)
	require.True(t, errors.Is(err, cortex.ErrNoScope), "got %v", err)
	require.True(t, errors.Is(err, weave.ErrTenantRequired), "got %v", err)
}

func TestCollectionNameResolvesInTenant(t *testing.T) {
	f := newFixture(t)
	a := weaveknowledge.New(f.eng)

	got, err := a.Retrieve(scoped("o1"), "restart the worker", &knowledge.RetrieveParams{Collection: "runbooks", TopK: 10})
	require.NoError(t, err)
	require.NotEmpty(t, got)
	for _, c := range got {
		require.Equal(t, f.cols["o1/runbooks"], c.CollectionID)
	}
	require.NotContains(t, joined(got), "secret")

	got, err = a.Retrieve(scoped("o2"), "restart the worker", &knowledge.RetrieveParams{Collection: "runbooks", TopK: 10})
	require.NoError(t, err)
	require.Contains(t, joined(got), "secret")
	require.NotContains(t, joined(got), "payments")

	// o1 cannot reach o2's collection by ID either.
	_, err = a.Retrieve(scoped("o1"), "restart", &knowledge.RetrieveParams{Collection: f.cols["o2/runbooks"]})
	require.Error(t, err)
}

func TestSharedCollectionIsSearchedToo(t *testing.T) {
	f := newFixture(t)
	a := weaveknowledge.New(f.eng, weaveknowledge.WithShared(weaveknowledge.SharedCollection{Tenant: "docs", Collection: "loomplane-docs"}))

	got, err := a.Retrieve(scoped("o1"), "restart the worker", &knowledge.RetrieveParams{TopK: 10})
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Contains(t, joined(got), "payments")
	require.Contains(t, joined(got), "dashboard")
	require.NotContains(t, joined(got), "secret")
	require.GreaterOrEqual(t, got[0].Score, got[1].Score)

	cut, err := a.Retrieve(scoped("o1"), "restart the worker", &knowledge.RetrieveParams{TopK: 1})
	require.NoError(t, err)
	require.Len(t, cut, 1)
	require.Equal(t, got[0].Content, cut[0].Content)
}

func TestNamedSharedCollection(t *testing.T) {
	f := newFixture(t)
	a := weaveknowledge.New(f.eng, weaveknowledge.WithShared(weaveknowledge.SharedCollection{Tenant: "docs", Collection: "loomplane-docs"}))

	got, err := a.Retrieve(scoped("o1"), "restart the worker", &knowledge.RetrieveParams{Collection: "loomplane-docs", TopK: 10})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Contains(t, got[0].Content, "dashboard")
	require.Equal(t, f.cols["docs/loomplane-docs"], got[0].CollectionID)
}

func TestListCollectionsPerTenant(t *testing.T) {
	f := newFixture(t)
	a := weaveknowledge.New(f.eng, weaveknowledge.WithShared(weaveknowledge.SharedCollection{Tenant: "docs", Collection: "loomplane-docs"}))

	got, err := a.ListCollections(scoped("o1"))
	require.NoError(t, err)
	ids := map[string]knowledge.CollectionInfo{}
	for _, c := range got {
		ids[c.ID] = c
	}
	require.Len(t, got, 2)
	require.Contains(t, ids, f.cols["o1/runbooks"])
	require.Contains(t, ids, f.cols["docs/loomplane-docs"])
	require.NotContains(t, ids, f.cols["o2/runbooks"])
	require.EqualValues(t, 1, ids[f.cols["o1/runbooks"]].DocumentCount)
	require.EqualValues(t, 1, ids[f.cols["docs/loomplane-docs"]].DocumentCount)
}

func TestOtherTenantsCollectionIDIsRefused(t *testing.T) {
	f := newFixture(t)
	a := weaveknowledge.New(f.eng)
	foreign := f.cols["o2/runbooks"]
	require.NotEmpty(t, foreign)

	// A run in o1 that names o2's collection by its parsed ID gets nothing
	// of o2's: weave answers as if the collection did not exist.
	got, err := a.Retrieve(scoped("o1"), "restart the secret worker", &knowledge.RetrieveParams{Collection: foreign, TopK: 10})
	require.ErrorIs(t, err, weave.ErrCollectionNotFound)
	require.Empty(t, got)
	require.NotContains(t, err.Error(), "secret worker")

	// o2 itself still reads it.
	got, err = a.Retrieve(scoped("o2"), "restart the secret worker", &knowledge.RetrieveParams{Collection: foreign, TopK: 10})
	require.NoError(t, err)
	require.Contains(t, joined(got), "secret")
}
