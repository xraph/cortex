// Package nexus adapts the Nexus LLM gateway to the cortex llm.Client interface.
//
// Every call goes out under a nexus tenant and carries usage metadata: the
// tenant comes from the context (pipeline.WithTenantID) or from a
// cortex.TenantFunc over the context's scope, and the metadata names the
// scope levels and the run and agent the engine put on the request. A call
// with no tenant is refused before anything is sent.
package nexus

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"

	"github.com/xraph/nexus"
	"github.com/xraph/nexus/guard"
	"github.com/xraph/nexus/pipeline"
	"github.com/xraph/nexus/provider"
	"github.com/xraph/nexus/usage"
	"github.com/xraph/vessel"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/llm"
)

// ErrToolCallWithoutID reports a streamed tool-call fragment that names no
// call and follows no fragment that did, so it cannot be attached to one.
var ErrToolCallWithoutID = errors.New("llm: tool-call fragment without an id")

// samplingUnsupported lists the models that reject temperature and top_p.
var samplingUnsupported = []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5"}

// metadataKey is the key format nexus accepts in usage metadata.
var metadataKey = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Option configures an Adapter.
type Option func(*Adapter)

// WithTenant sets how the adapter names the nexus tenant for a call whose
// context carries none (pipeline.WithTenantID wins when it is set). The
// result must be a nexus tenant ID. The default is cortex.RootTenant, the
// value of the scope's first level, which only works when that value is
// the nexus tenant ID.
func WithTenant(f cortex.TenantFunc) Option {
	return func(a *Adapter) {
		if f != nil {
			a.tenant = f
		}
	}
}

// WithMetadata replaces how the adapter builds a call's usage metadata. f
// gets the context's scope and a copy of the request's Metadata (the run
// and agent the engine set). The default returns one pair per scope level,
// level key to value, plus those pairs. Whatever f returns is then cut to
// nexus's rules: pairs with a key outside ^[A-Za-z0-9_-]{1,64}$ or a value
// over 256 bytes are dropped, and at most 16 are kept, run and agent first.
func WithMetadata(f func(ctx context.Context, s cortex.Scope, md map[string]string) map[string]string) Option {
	return func(a *Adapter) {
		if f != nil {
			a.metadata = f
		}
	}
}

// WithSamplingUnsupported sets which models reject sampling parameters.
// For those the adapter sends no temperature (and it never sends top_p).
// The default knows claude-opus-5-5, claude-sonnet-5-5 and
// claude-haiku-5-5; a host that calls models through nexus aliases passes
// a predicate that knows what each alias points at.
func WithSamplingUnsupported(f func(model string) bool) Option {
	return func(a *Adapter) {
		if f != nil {
			a.samplingUnsupported = f
		}
	}
}

// WithProvider names the nexus provider a call goes to, from the context's
// scope. An empty result leaves the choice to nexus, which routes by alias
// and then by the providers that list the model, so a model id two
// providers share can go to either.
func WithProvider(f func(ctx context.Context, s cortex.Scope) string) Option {
	return func(a *Adapter) { a.provider = f }
}

// Adapter implements llm.Client by delegating to a nexus gateway's engine.
type Adapter struct {
	gateway             func() *nexus.Gateway
	tenant              cortex.TenantFunc
	metadata            func(ctx context.Context, s cortex.Scope, md map[string]string) map[string]string
	samplingUnsupported func(model string) bool
	provider            func(ctx context.Context, s cortex.Scope) string
}

// New creates an adapter over gw. Calls made before gw.Initialize returns
// fail with nexus.ErrNotInitialized.
func New(gw *nexus.Gateway, opts ...Option) *Adapter {
	return NewFunc(func() *nexus.Gateway { return gw }, opts...)
}

// NewFunc creates an adapter that asks gw for the gateway on every call,
// for a gateway that does not exist yet when the adapter is built, such as
// the one a nexus Forge extension creates when it starts. While gw returns
// nil, calls fail with nexus.ErrNotInitialized.
func NewFunc(gw func() *nexus.Gateway, opts ...Option) *Adapter {
	a := &Adapter{
		gateway:             gw,
		tenant:              cortex.RootTenant,
		metadata:            scopeMetadata,
		samplingUnsupported: func(model string) bool { return slices.Contains(samplingUnsupported, model) },
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// EngineOption returns an engine.Option that finds a *nexus.Gateway in the
// DI container (nexus's Forge extension provides one) and makes an adapter
// over it, with opts, the engine's LLM client. With no gateway in the
// container it does nothing, so it is safe to always include.
func EngineOption(c vessel.Vessel, opts ...Option) engine.Option {
	gw, err := vessel.Inject[*nexus.Gateway](c)
	if err != nil {
		return func(_ *engine.Engine) error { return nil }
	}
	return engine.WithLLM(New(gw, opts...))
}

// Complete sends a synchronous completion request via the Nexus pipeline.
func (a *Adapter) Complete(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	ctx, nReq, err := a.prepare(ctx, req)
	if err != nil {
		return nil, err
	}
	eng, err := a.engine()
	if err != nil {
		return nil, err
	}
	resp, err := eng.Complete(ctx, nReq)
	if err != nil {
		return nil, blocked(err)
	}
	return fromNexusResponse(resp), nil
}

// CompleteStream sends a streaming completion request via the Nexus pipeline.
func (a *Adapter) CompleteStream(ctx context.Context, req *llm.Request) (llm.Stream, error) {
	ctx, nReq, err := a.prepare(ctx, req)
	if err != nil {
		return nil, err
	}
	nReq.Stream = true
	eng, err := a.engine()
	if err != nil {
		return nil, err
	}
	stream, err := eng.CompleteStream(ctx, nReq)
	if err != nil {
		return nil, blocked(err)
	}
	return &streamAdapter{stream: stream}, nil
}

func (a *Adapter) engine() (*nexus.Engine, error) {
	var gw *nexus.Gateway
	if a.gateway != nil {
		gw = a.gateway()
	}
	if gw == nil || gw.Engine() == nil {
		return nil, nexus.ErrNotInitialized
	}
	return gw.Engine(), nil
}

// blocked wraps a guard's refusal in llm.ErrContentBlocked and returns any
// other error as it is.
func blocked(err error) error {
	if be, ok := errors.AsType[*guard.BlockedError](err); ok && be != nil {
		return fmt.Errorf("%w: %w", llm.ErrContentBlocked, err)
	}
	return err
}

// prepare resolves the call's tenant onto ctx and builds the nexus request.
func (a *Adapter) prepare(ctx context.Context, req *llm.Request) (context.Context, *provider.CompletionRequest, error) {
	scope := cortex.ScopeFromContext(ctx)
	if pipeline.TenantID(ctx) == "" {
		tenant, err := a.tenant(ctx, scope)
		if err != nil {
			return nil, nil, fmt.Errorf("llm: nexus tenant: %w", err)
		}
		if tenant == "" {
			return nil, nil, fmt.Errorf("llm: nexus tenant: %w", cortex.ErrNoScope)
		}
		ctx = pipeline.WithTenantID(ctx, tenant)
	}

	toolChoice, err := toNexusToolChoice(req.ToolChoice)
	if err != nil {
		return nil, nil, err
	}
	metadata, err := sanitizeMetadata(a.metadata(ctx, scope, maps.Clone(req.Metadata)))
	if err != nil {
		return nil, nil, err
	}

	nReq := &provider.CompletionRequest{
		Model:      req.Model,
		System:     req.System,
		MaxTokens:  req.MaxTokens,
		Messages:   toNexusMessages(req.Messages),
		Tools:      toNexusTools(req.Tools),
		ToolChoice: toolChoice,
		Metadata:   metadata,
	}
	if req.Temperature != nil && !a.samplingUnsupported(req.Model) {
		t := *req.Temperature
		nReq.Temperature = &t
	}
	if a.provider != nil {
		nReq.Provider = a.provider(ctx, scope)
	}
	return ctx, nReq, nil
}

// scopeMetadata is the default metadata: one pair per scope level, then the
// request's own pairs.
func scopeMetadata(_ context.Context, s cortex.Scope, md map[string]string) map[string]string {
	out := make(map[string]string, len(s.Levels)+len(md))
	for _, l := range s.Levels {
		out[l.Key] = l.Value
	}
	maps.Copy(out, md)
	return out
}

// sanitizeMetadata keeps the pairs nexus accepts: valid keys, short values,
// at most usage.MaxMetadataPairs, the run and the agent first and the rest
// in key order.
func sanitizeMetadata(md map[string]string) (map[string]string, error) {
	keys := slices.Sorted(maps.Keys(md))
	slices.SortStableFunc(keys, func(x, y string) int { return metadataRank(x) - metadataRank(y) })
	out := make(map[string]string, min(len(keys), usage.MaxMetadataPairs))
	for _, k := range keys {
		if len(out) == usage.MaxMetadataPairs {
			break
		}
		if v := md[k]; metadataKey.MatchString(k) && len(v) <= usage.MaxMetadataValueBytes {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	if err := usage.ValidateMetadata(out); err != nil {
		return nil, fmt.Errorf("llm: nexus metadata: %w", err)
	}
	return out, nil
}

func metadataRank(key string) int {
	switch key {
	case llm.MetadataRun:
		return 0
	case llm.MetadataAgent:
		return 1
	}
	return 2
}

// toNexusToolChoice converts a tool choice to OpenAI's shape, which nexus
// reads with provider.ParseToolChoice. A mode nexus does not know, or a
// function choice with no function, is refused here with an error that
// wraps provider.ErrNotSupported, as nexus would refuse it.
func toNexusToolChoice(tc *llm.ToolChoice) (any, error) {
	if tc == nil {
		return nil, nil
	}
	choice := provider.ToolChoice{Mode: provider.ToolChoiceMode(tc.Mode), Function: tc.Function}
	if choice.Mode == provider.ToolChoiceFunction && choice.Function == "" {
		return nil, fmt.Errorf("llm: tool choice function has no name: %w", provider.ErrNotSupported)
	}
	v := choice.OpenAI()
	if v == nil {
		return nil, fmt.Errorf("llm: tool choice mode %q: %w", tc.Mode, provider.ErrNotSupported)
	}
	return v, nil
}

// ──────────────────────────────────────────────────
// Request conversion: llm.Request → provider.CompletionRequest
// ──────────────────────────────────────────────────

func toNexusMessages(msgs []llm.Message) []provider.Message {
	out := make([]provider.Message, len(msgs))
	for i, m := range msgs {
		out[i] = provider.Message{
			Role:       m.Role,
			Content:    m.Content, // string → any (provider.Message.Content is any)
			ToolCalls:  toNexusToolCalls(m.ToolCalls),
			ToolCallID: m.ToolCallID,
			Native:     toNexusNative(m.Native),
		}
	}
	return out
}

func toNexusNative(n *llm.NativeContent) *provider.NativeContent {
	if n == nil {
		return nil
	}
	c := n.Clone()
	return &provider.NativeContent{Provider: c.Provider, Model: c.Model, Content: c.Content}
}

func toNexusTools(tools []llm.Tool) []provider.Tool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]provider.Tool, len(tools))
	for i, t := range tools {
		out[i] = provider.Tool{
			Type: "function",
			Function: provider.ToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		}
	}
	return out
}

func toNexusToolCalls(tcs []llm.ToolCall) []provider.ToolCall {
	if len(tcs) == 0 {
		return nil
	}
	out := make([]provider.ToolCall, len(tcs))
	for i, tc := range tcs {
		out[i] = provider.ToolCall{
			ID:   tc.ID,
			Type: "function",
			Function: provider.ToolCallFunc{
				Name:      tc.Name,
				Arguments: tc.Arguments,
			},
		}
	}
	return out
}

// ──────────────────────────────────────────────────
// Response conversion: provider.CompletionResponse → llm.Response
// ──────────────────────────────────────────────────

func fromNexusResponse(resp *provider.CompletionResponse) *llm.Response {
	r := &llm.Response{
		Model: resp.Model,
		Usage: llm.Usage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		},
	}
	if len(resp.Choices) > 0 {
		choice := resp.Choices[0]
		r.Content = messageContentString(choice.Message.Content)
		r.ToolCalls = fromNexusToolCalls(choice.Message.ToolCalls)
		r.FinishReason = choice.FinishReason
		r.Native = fromNexusNative(choice.Message.Native)
	}
	return r
}

func fromNexusNative(n *provider.NativeContent) *llm.NativeContent {
	if n == nil {
		return nil
	}
	return (&llm.NativeContent{Provider: n.Provider, Model: n.Model, Content: n.Content}).Clone()
}

func fromNexusToolCalls(tcs []provider.ToolCall) []llm.ToolCall {
	if len(tcs) == 0 {
		return nil
	}
	out := make([]llm.ToolCall, len(tcs))
	for i, tc := range tcs {
		out[i] = llm.ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		}
	}
	return out
}

// messageContentString extracts a string from provider.Message.Content,
// which can be a string or []ContentPart.
func messageContentString(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		// Try to extract text parts.
		var parts []string
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		if len(parts) > 0 {
			result := parts[0]
			for i := 1; i < len(parts); i++ {
				result += parts[i]
			}
			return result
		}
	}
	return ""
}

// ──────────────────────────────────────────────────
// Stream adapter: provider.Stream → llm.Stream
// ──────────────────────────────────────────────────

type streamAdapter struct {
	stream provider.Stream
	// lastID is the last tool-call ID the stream named. A fragment with no
	// ID continues that call.
	lastID string
}

func (s *streamAdapter) Next(ctx context.Context) (*llm.Chunk, error) {
	chunk, err := s.stream.Next(ctx)
	if err != nil {
		return nil, blocked(err) // includes io.EOF, unchanged
	}
	calls := fromNexusToolCalls(chunk.Delta.ToolCalls)
	for i := range calls {
		switch {
		case calls[i].ID != "":
			s.lastID = calls[i].ID
		case s.lastID != "":
			calls[i].ID = s.lastID
		default:
			return nil, ErrToolCallWithoutID
		}
	}
	return &llm.Chunk{
		Content:      chunk.Delta.Content,
		ToolCalls:    calls,
		FinishReason: chunk.FinishReason,
		Native:       fromNexusNative(chunk.Delta.Native),
	}, nil
}

func (s *streamAdapter) Close() error {
	return s.stream.Close()
}

func (s *streamAdapter) Usage() *llm.Usage {
	u := s.stream.Usage()
	if u == nil {
		return nil
	}
	return &llm.Usage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
	}
}
