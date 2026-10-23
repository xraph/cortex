package engine

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/safety"
	"github.com/xraph/cortex/verification"
)

// ResultContentConfig supplies current content grants and resource/policy fencing.
// It is independent of metadata replay permission and verification worker authority.
// The fence must cover current agent safety-profile changes as policy mutations.
type ResultContentConfig struct {
	Authorizer verification.ResultContentAuthorizer
	Fence      verification.CommitFence
	Clock      verification.Clock
}

// WithResultContent enables screened reads of committed verified results. A current
// publication gate, enabled publication policy and snapshot-capable store are required.
func WithResultContent(config ResultContentConfig) Option {
	return func(e *Engine) error { copied := config; e.resultContentConfig = &copied; return nil }
}

func (e *Engine) configureResultContent() error {
	if e.resultContentConfig == nil {
		return nil
	}
	storage, ok := e.store.(verification.ResultContentStore)
	if !ok || e.publicationGate == nil || e.publicationPolicy.Mode == safety.ModeDisabled {
		return safety.ErrUnavailable
	}
	config := *e.resultContentConfig
	reader, err := verification.NewResultReader(verification.ResultReaderConfig{Store: storage, Authorizer: config.Authorizer, Fence: config.Fence, Publisher: resultContentPublisher{engine: e}, Clock: config.Clock})
	if err != nil {
		return err
	}
	e.resultContentReader = reader
	e.resultContentConfig = nil
	return nil
}

// ResultContentAvailable reports whether current-authority content reads are configured.
func (e *Engine) ResultContentAvailable() bool { return e != nil && e.resultContentReader != nil }

// ReadResultContent returns a freshly screened projection with minimal metadata.
// A redacted projection is marked ContentChanged; verification describes the
// original candidate. This read does not regenerate, reevaluate or persist output.
func (e *Engine) ReadResultContent(ctx context.Context, request verification.ResultContentRequest) (verification.PublishedResult, error) {
	if !e.ResultContentAvailable() {
		return verification.PublishedResult{}, safety.ErrUnavailable
	}
	return e.resultContentReader.Read(ctx, request)
}

type resultContentPublisher struct{ engine *Engine }

func (p resultContentPublisher) PublishResult(ctx context.Context, request verification.ResultPublicationRequest) (safety.PublicationDecision, error) {
	e := p.engine
	current, err := e.store.GetRun(ctx, request.Request.RunID)
	if err != nil {
		return safety.PublicationDecision{}, err
	}
	scope := cortex.ScopeFromContext(ctx)
	if current == nil || current.ID != request.Request.RunID || current.Revision != request.RunRevision || current.Scope.Canonical() != scope.Canonical() || current.Provenance.PreparedDigest != request.Binding.Context.PreparedDigest {
		return safety.PublicationDecision{}, verification.ErrConflict
	}
	ag, err := e.store.Get(ctx, current.AgentID)
	if err != nil {
		return safety.PublicationDecision{}, err
	}
	if requireAgentScope(ctx, ag) != nil || ag.ID != current.AgentID {
		return safety.PublicationDecision{}, verification.ErrConflict
	}
	// Only this check's status belongs to the returned projection. Historical
	// publication flags remain unchanged in the stored run.
	projection := *current
	projection.Provenance.PublicationStatus = ""
	text, err := e.checkPublication(ctx, ag, &projection, e.publicationPolicy, request.Content, "result_replay", safety.DirectionOutput)
	if err != nil {
		return safety.PublicationDecision{}, err
	}
	return safety.PublicationDecision{Text: string(text), Status: projection.Provenance.PublicationStatus}, nil
}
