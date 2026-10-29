package nexus_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/xraph/nexus"
	"github.com/xraph/nexus/guard"
	"github.com/xraph/nexus/pipeline"
	"github.com/xraph/nexus/provider"
	"github.com/xraph/nexus/tenant"
	"github.com/xraph/nexus/usage"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/llm"
	nexusllm "github.com/xraph/cortex/llm/nexus"
)

// fakeProvider is a nexus provider that records every request it gets and
// the tenant the pipeline handed it, and answers with what the test set.
type fakeProvider struct {
	name   string
	models []string

	mu       sync.Mutex
	requests []*provider.CompletionRequest
	tenants  []string

	response  *provider.CompletionResponse
	err       error
	chunks    []*provider.StreamChunk
	streamErr error
}

func newFake(name string, models ...string) *fakeProvider {
	if len(models) == 0 {
		models = []string{"m"}
	}
	return &fakeProvider{name: name, models: models}
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{Chat: true, Streaming: true, Tools: true, Thinking: true, StreamingTools: true}
}

func (f *fakeProvider) Models(context.Context) ([]provider.Model, error) {
	out := make([]provider.Model, 0, len(f.models))
	for _, m := range f.models {
		out = append(out, provider.Model{ID: m, Provider: f.name, Name: m})
	}
	return out, nil
}

func (f *fakeProvider) record(ctx context.Context, req *provider.CompletionRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	f.tenants = append(f.tenants, pipeline.TenantID(ctx))
}

func (f *fakeProvider) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	f.record(ctx, req)
	if f.err != nil {
		return nil, f.err
	}
	if f.response != nil {
		return f.response, nil
	}
	return &provider.CompletionResponse{
		Model:   req.Model,
		Choices: []provider.Choice{{Message: provider.Message{Role: "assistant", Content: "ok"}, FinishReason: "stop"}},
	}, nil
}

func (f *fakeProvider) CompleteStream(ctx context.Context, req *provider.CompletionRequest) (provider.Stream, error) {
	f.record(ctx, req)
	if f.err != nil {
		return nil, f.err
	}
	return &fakeStream{chunks: f.chunks, err: f.streamErr}, nil
}

func (f *fakeProvider) Embed(context.Context, *provider.EmbeddingRequest) (*provider.EmbeddingResponse, error) {
	return nil, provider.ErrNotSupported
}

func (f *fakeProvider) Healthy(context.Context) bool { return true }

func (f *fakeProvider) seen() ([]*provider.CompletionRequest, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*provider.CompletionRequest(nil), f.requests...), append([]string(nil), f.tenants...)
}

// fakeStream yields its chunks, then err (io.EOF when err is nil).
type fakeStream struct {
	chunks []*provider.StreamChunk
	err    error
	next   int
}

func (s *fakeStream) Next(context.Context) (*provider.StreamChunk, error) {
	if s.next < len(s.chunks) {
		c := s.chunks[s.next]
		s.next++
		return c, nil
	}
	if s.err != nil {
		return nil, s.err
	}
	return nil, io.EOF
}

func (s *fakeStream) Close() error           { return nil }
func (s *fakeStream) Usage() *provider.Usage { return &provider.Usage{TotalTokens: 3} }

// gateway starts a nexus gateway over the given providers.
func gateway(t *testing.T, providers ...provider.Provider) *nexus.Gateway {
	t.Helper()
	opts := make([]nexus.Option, 0, len(providers))
	for _, p := range providers {
		opts = append(opts, nexus.WithProvider(p))
	}
	gw := nexus.New(opts...)
	require.NoError(t, gw.Initialize(context.Background()))
	t.Cleanup(func() { _ = gw.Shutdown(context.Background()) })
	return gw
}

// newTenant creates an active nexus tenant and returns its ID.
func newTenant(t *testing.T, gw *nexus.Gateway, slug string) string {
	t.Helper()
	tn, err := gw.Tenants().Create(context.Background(), &tenant.CreateInput{Name: slug, Slug: slug})
	require.NoError(t, err)
	return tn.ID.String()
}

func scoped(levels ...string) context.Context {
	s := cortex.Scope{}
	for i := 0; i+1 < len(levels); i += 2 {
		s.Levels = append(s.Levels, cortex.Level{Key: levels[i], Value: levels[i+1]})
	}
	return cortex.WithScope(context.Background(), s)
}

// fixedTenant is a TenantFunc that always names tenant.
func fixedTenant(tenant string) cortex.TenantFunc {
	return func(context.Context, cortex.Scope) (string, error) { return tenant, nil }
}

func userRequest(model string) *llm.Request {
	return &llm.Request{Model: model, Messages: []llm.Message{{Role: "user", Content: "hi"}}}
}

func TestCompleteSetsTenantAndMetadata(t *testing.T) {
	fake := newFake("a")
	gw := gateway(t, fake)
	tenantX := newTenant(t, gw, "x")
	a := nexusllm.New(gw, nexusllm.WithTenant(func(_ context.Context, s cortex.Scope) (string, error) {
		if org, _ := s.Get("org"); org == "o1" {
			return tenantX, nil
		}
		return "", errors.New("unknown org")
	}))

	req := userRequest("m")
	req.Metadata = map[string]string{llm.MetadataRun: "arun_1", llm.MetadataAgent: "agt_1"}
	resp, err := a.Complete(scoped("org", "o1", "project", "p1"), req)
	require.NoError(t, err)
	require.Equal(t, "ok", resp.Content)

	reqs, tenants := fake.seen()
	require.Len(t, reqs, 1)
	require.Equal(t, tenantX, tenants[0])
	require.Equal(t, map[string]string{"org": "o1", "project": "p1", "run": "arun_1", "agent": "agt_1"}, reqs[0].Metadata)
	require.Equal(t, map[string]string{llm.MetadataRun: "arun_1", llm.MetadataAgent: "agt_1"}, req.Metadata, "the caller's metadata was changed")
}

func TestContextTenantWins(t *testing.T) {
	fake := newFake("a")
	gw := gateway(t, fake)
	tenantY := newTenant(t, gw, "y")
	called := false
	a := nexusllm.New(gw, nexusllm.WithTenant(func(context.Context, cortex.Scope) (string, error) {
		called = true
		return "", errors.New("must not be asked")
	}))

	ctx := pipeline.WithTenantID(scoped("org", "o1"), tenantY)
	_, err := a.Complete(ctx, userRequest("m"))
	require.NoError(t, err)
	require.False(t, called, "the TenantFunc ran although ctx named a tenant")
	_, tenants := fake.seen()
	require.Equal(t, []string{tenantY}, tenants)
}

func TestProviderOptionRoutes(t *testing.T) {
	cases := []struct {
		name string
		opt  []nexusllm.Option
		want string
	}{
		{"names b", []nexusllm.Option{nexusllm.WithProvider(func(context.Context, cortex.Scope) string { return "b" })}, "b"},
		{"empty", []nexusllm.Option{nexusllm.WithProvider(func(context.Context, cortex.Scope) string { return "" })}, ""},
		{"no option", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fa, fb := newFake("a"), newFake("b")
			gw := gateway(t, fa, fb)
			tn := newTenant(t, gw, "routes")
			opts := append([]nexusllm.Option{nexusllm.WithTenant(fixedTenant(tn))}, tc.opt...)
			_, err := nexusllm.New(gw, opts...).Complete(scoped("org", "o1"), userRequest("m"))
			require.NoError(t, err)

			ra, _ := fa.seen()
			rb, _ := fb.seen()
			require.Len(t, append(ra, rb...), 1, "exactly one provider gets the request")
			if tc.want == "b" {
				require.Empty(t, ra)
				require.Equal(t, "b", rb[0].Provider)
				return
			}
			require.Empty(t, append(ra, rb...)[0].Provider)
		})
	}
}

func TestCompleteWithoutTenantSendsNothing(t *testing.T) {
	fake := newFake("a")
	gw := gateway(t, fake)

	_, err := nexusllm.New(gw).Complete(context.Background(), userRequest("m"))
	require.ErrorIs(t, err, cortex.ErrNoScope)
	_, err = nexusllm.New(gw).CompleteStream(context.Background(), userRequest("m"))
	require.ErrorIs(t, err, cortex.ErrNoScope)

	// A TenantFunc that names nobody is the same as no scope.
	_, err = nexusllm.New(gw, nexusllm.WithTenant(fixedTenant(""))).Complete(scoped("org", "o1"), userRequest("m"))
	require.ErrorIs(t, err, cortex.ErrNoScope)

	reqs, _ := fake.seen()
	require.Empty(t, reqs)
}

func TestMetadataIsSanitized(t *testing.T) {
	t.Run("invalid pairs dropped", func(t *testing.T) {
		fake := newFake("a")
		gw := gateway(t, fake)
		tn := newTenant(t, gw, "md")
		ctx := scoped("org", "o1", "bad key", "x", "project", strings.Repeat("p", 300))
		req := userRequest("m")
		req.Metadata = map[string]string{llm.MetadataRun: "arun_1", llm.MetadataAgent: "agt_1"}
		_, err := nexusllm.New(gw, nexusllm.WithTenant(fixedTenant(tn))).Complete(ctx, req)
		require.NoError(t, err)
		reqs, _ := fake.seen()
		require.Equal(t, map[string]string{"org": "o1", "run": "arun_1", "agent": "agt_1"}, reqs[0].Metadata)
	})

	t.Run("cut to sixteen", func(t *testing.T) {
		fake := newFake("a")
		gw := gateway(t, fake)
		tn := newTenant(t, gw, "many")
		many := nexusllm.WithMetadata(func(_ context.Context, _ cortex.Scope, md map[string]string) map[string]string {
			out := map[string]string{}
			for i := range 20 {
				out[fmt.Sprintf("level_%02d", i)] = "v"
			}
			for k, v := range md {
				out[k] = v
			}
			return out
		})
		req := userRequest("m")
		req.Metadata = map[string]string{llm.MetadataRun: "arun_1", llm.MetadataAgent: "agt_1"}
		_, err := nexusllm.New(gw, nexusllm.WithTenant(fixedTenant(tn)), many).Complete(scoped("org", "o1"), req)
		require.NoError(t, err)
		reqs, _ := fake.seen()
		md := reqs[0].Metadata
		require.Len(t, md, usage.MaxMetadataPairs)
		require.Equal(t, "arun_1", md["run"])
		require.Equal(t, "agt_1", md["agent"])
		require.NoError(t, usage.ValidateMetadata(md))
	})
}

func TestSamplingOmittedForUnsupportedModels(t *testing.T) {
	fake := newFake("a", "claude-sonnet-5-5", "claude-sonnet-4-6")
	gw := gateway(t, fake)
	tn := newTenant(t, gw, "sampling")
	a := nexusllm.New(gw, nexusllm.WithTenant(fixedTenant(tn)))
	temp := 0.7

	for _, model := range []string{"claude-sonnet-5-5", "claude-sonnet-4-6"} {
		req := userRequest(model)
		req.Temperature = &temp
		_, err := a.Complete(scoped("org", "o1"), req)
		require.NoError(t, err)
	}
	reqs, _ := fake.seen()
	require.Len(t, reqs, 2)
	require.Nil(t, reqs[0].Temperature, "claude-sonnet-5-5 rejects temperature")
	require.NotNil(t, reqs[1].Temperature)
	require.InDelta(t, 0.7, *reqs[1].Temperature, 0)
	for _, r := range reqs {
		require.Nil(t, r.TopP)
	}

	// A host predicate replaces the default list.
	custom := nexusllm.New(gw, nexusllm.WithTenant(fixedTenant(tn)),
		nexusllm.WithSamplingUnsupported(func(model string) bool { return model == "claude-sonnet-4-6" }))
	for _, model := range []string{"claude-sonnet-5-5", "claude-sonnet-4-6"} {
		req := userRequest(model)
		req.Temperature = &temp
		_, err := custom.Complete(scoped("org", "o1"), req)
		require.NoError(t, err)
	}
	reqs, _ = fake.seen()
	require.NotNil(t, reqs[2].Temperature)
	require.Nil(t, reqs[3].Temperature)
}

func TestToolChoiceMapping(t *testing.T) {
	fake := newFake("a")
	gw := gateway(t, fake)
	tn := newTenant(t, gw, "choice")
	a := nexusllm.New(gw, nexusllm.WithTenant(fixedTenant(tn)))
	tools := []llm.Tool{{Name: "deploy", Description: "deploys", Parameters: map[string]any{"type": "object"}}}

	choices := []llm.ToolChoice{
		{Mode: "auto"},
		{Mode: "none"},
		{Mode: "required"},
		{Mode: "function", Function: "deploy"},
	}
	for _, choice := range choices {
		req := userRequest("m")
		req.Tools = tools
		req.ToolChoice = &choice
		_, err := a.Complete(scoped("org", "o1"), req)
		require.NoError(t, err)
	}
	req := userRequest("m")
	req.Tools = tools
	_, err := a.Complete(scoped("org", "o1"), req)
	require.NoError(t, err)

	reqs, _ := fake.seen()
	require.Len(t, reqs, len(choices)+1)
	for i, choice := range choices {
		parsed, ok, parseErr := provider.ParseToolChoice(reqs[i].ToolChoice)
		require.NoError(t, parseErr)
		require.True(t, ok)
		require.Equal(t, provider.ToolChoiceMode(choice.Mode), parsed.Mode)
		require.Equal(t, choice.Function, parsed.Function)
	}
	require.Nil(t, reqs[len(choices)].ToolChoice, "no ToolChoice sends no tool_choice")

	// A mode nexus does not know is refused here, before anything is sent.
	for _, bad := range []llm.ToolChoice{{Mode: "sometimes"}, {Mode: "function"}} {
		req := userRequest("m")
		req.ToolChoice = &bad
		_, err = a.Complete(scoped("org", "o1"), req)
		require.ErrorIs(t, err, provider.ErrNotSupported)
	}
	reqs, _ = fake.seen()
	require.Len(t, reqs, len(choices)+1)
}

func TestNativeContentRoundTrip(t *testing.T) {
	native := &provider.NativeContent{Provider: "anthropic", Model: "m", Content: json.RawMessage(`[{"type":"thinking","signature":"s"}]`)}
	fake := newFake("a")
	fake.response = &provider.CompletionResponse{
		Model: "m",
		Choices: []provider.Choice{{
			Message:      provider.Message{Role: "assistant", Content: "", ToolCalls: []provider.ToolCall{{ID: "c1", Type: "function", Function: provider.ToolCallFunc{Name: "x", Arguments: "{}"}}}, Native: native},
			FinishReason: "tool_calls",
		}},
	}
	gw := gateway(t, fake)
	tn := newTenant(t, gw, "native")
	a := nexusllm.New(gw, nexusllm.WithTenant(fixedTenant(tn)))

	resp, err := a.Complete(scoped("org", "o1"), userRequest("m"))
	require.NoError(t, err)
	require.NotNil(t, resp.Native)
	require.Equal(t, "anthropic", resp.Native.Provider)
	require.Equal(t, "m", resp.Native.Model)
	require.Equal(t, []byte(native.Content), []byte(resp.Native.Content))

	req := userRequest("m")
	req.Messages = append(req.Messages,
		llm.Message{Role: "assistant", ToolCalls: resp.ToolCalls, Native: resp.Native},
		llm.Message{Role: "tool", Content: "done", ToolCallID: "c1"},
	)
	_, err = a.Complete(scoped("org", "o1"), req)
	require.NoError(t, err)
	reqs, _ := fake.seen()
	sent := reqs[1].Messages[1]
	require.NotNil(t, sent.Native)
	require.Equal(t, "anthropic", sent.Native.Provider)
	require.Equal(t, "m", sent.Native.Model)
	require.Equal(t, []byte(native.Content), []byte(sent.Native.Content))
	require.Nil(t, reqs[1].Messages[0].Native)
	require.Nil(t, reqs[1].Messages[2].Native)
}

func toolChunk(id, name, args string) *provider.StreamChunk {
	return &provider.StreamChunk{Delta: provider.Delta{ToolCalls: []provider.ToolCall{{ID: id, Type: "function", Function: provider.ToolCallFunc{Name: name, Arguments: args}}}}}
}

// drain reads s to its end and returns the chunks and the error that ended
// it (nil for io.EOF).
func drain(t *testing.T, s llm.Stream) ([]*llm.Chunk, error) {
	t.Helper()
	defer func() { _ = s.Close() }()
	var out []*llm.Chunk
	for {
		c, err := s.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, c)
	}
}

func TestStreamAssignsFragmentIDs(t *testing.T) {
	native := &provider.NativeContent{Provider: "anthropic", Model: "m", Content: json.RawMessage(`[{"type":"thinking","signature":"s"}]`)}
	fake := newFake("a")
	fake.chunks = []*provider.StreamChunk{
		toolChunk("c1", "x", `{"a"`),
		toolChunk("", "", `:1}`),
		toolChunk("c2", "y", `{}`),
		{FinishReason: "tool_calls", Delta: provider.Delta{Native: native}},
	}
	gw := gateway(t, fake)
	tn := newTenant(t, gw, "stream")
	s, err := nexusllm.New(gw, nexusllm.WithTenant(fixedTenant(tn))).CompleteStream(scoped("org", "o1"), userRequest("m"))
	require.NoError(t, err)
	chunks, err := drain(t, s)
	require.NoError(t, err)

	var ids []string
	for _, c := range chunks {
		for _, tc := range c.ToolCalls {
			ids = append(ids, tc.ID)
		}
	}
	require.Equal(t, []string{"c1", "c1", "c2"}, ids)
	last := chunks[len(chunks)-1]
	require.Equal(t, "tool_calls", last.FinishReason)
	require.NotNil(t, last.Native)
	require.Equal(t, []byte(native.Content), []byte(last.Native.Content))
	require.Equal(t, "anthropic", last.Native.Provider)
}

func TestStreamFragmentWithoutIDFails(t *testing.T) {
	fake := newFake("a")
	fake.chunks = []*provider.StreamChunk{toolChunk("", "x", `{}`), {FinishReason: "tool_calls"}}
	gw := gateway(t, fake)
	tn := newTenant(t, gw, "noid")
	s, err := nexusllm.New(gw, nexusllm.WithTenant(fixedTenant(tn))).CompleteStream(scoped("org", "o1"), userRequest("m"))
	require.NoError(t, err)
	chunks, err := drain(t, s)
	require.ErrorIs(t, err, nexusllm.ErrToolCallWithoutID)
	require.Empty(t, chunks)
}

func TestBlockedStreamSurfacesAsContentBlocked(t *testing.T) {
	fake := newFake("a")
	fake.chunks = []*provider.StreamChunk{toolChunk("c1", "x", `{}`)}
	fake.streamErr = &guard.BlockedError{Guard: "pii", Phase: guard.PhaseOutput, Reason: "blocked"}
	gw := gateway(t, fake)
	tn := newTenant(t, gw, "blocked")
	s, err := nexusllm.New(gw, nexusllm.WithTenant(fixedTenant(tn))).CompleteStream(scoped("org", "o1"), userRequest("m"))
	require.NoError(t, err)
	chunks, err := drain(t, s)
	require.Len(t, chunks, 1)
	require.ErrorIs(t, err, llm.ErrContentBlocked)
	var blocked *guard.BlockedError
	require.ErrorAs(t, err, &blocked)
	require.Equal(t, "pii", blocked.Guard)
}

func TestProviderErrorReturnedAsIs(t *testing.T) {
	refused := fmt.Errorf("fake: forced tool choice: %w", provider.ErrNotSupported)
	fake := newFake("a")
	fake.err = refused
	gw := gateway(t, fake)
	tn := newTenant(t, gw, "refused")
	req := userRequest("m")
	req.ToolChoice = &llm.ToolChoice{Mode: "function", Function: "x"}
	_, err := nexusllm.New(gw, nexusllm.WithTenant(fixedTenant(tn))).Complete(scoped("org", "o1"), req)
	require.ErrorIs(t, err, refused)
	require.NotErrorIs(t, err, llm.ErrContentBlocked)
}

func TestGatewayNotStarted(t *testing.T) {
	fake := newFake("a")
	gw := nexus.New(nexus.WithProvider(fake))
	ctx := pipeline.WithTenantID(scoped("org", "o1"), "tenant_unused")

	_, err := nexusllm.New(gw).Complete(ctx, userRequest("m"))
	require.ErrorIs(t, err, nexus.ErrNotInitialized)
	_, err = nexusllm.New(gw).CompleteStream(ctx, userRequest("m"))
	require.ErrorIs(t, err, nexus.ErrNotInitialized)

	// NewFunc before the extension has a gateway at all.
	_, err = nexusllm.NewFunc(func() *nexus.Gateway { return nil }).Complete(ctx, userRequest("m"))
	require.ErrorIs(t, err, nexus.ErrNotInitialized)

	// The same adapter works once the gateway starts.
	require.NoError(t, gw.Initialize(context.Background()))
	t.Cleanup(func() { _ = gw.Shutdown(context.Background()) })
	tn := newTenant(t, gw, "late")
	_, err = nexusllm.NewFunc(func() *nexus.Gateway { return gw }, nexusllm.WithTenant(fixedTenant(tn))).Complete(scoped("org", "o1"), userRequest("m"))
	require.NoError(t, err)
	reqs, _ := fake.seen()
	require.Len(t, reqs, 1)
}
