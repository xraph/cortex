package verification

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync/atomic"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/safety"
)

// PrefixExport identifies a host-reserved artifact export operation.
const PrefixExport id.Prefix = "vexport"

// MaxArtifactExportDuration bounds one attempt, including screening and delivery.
const MaxArtifactExportDuration = 30 * time.Second

// ErrExportUncertain means delivery may have occurred. Reconcile the same request.
var ErrExportUncertain = errors.New("cortex: artifact export outcome uncertain")

// ArtifactExportRequest pins the artifact, current approval and logical target.
// Destination is a host-registered identity, never a path or URL to resolve here.
// The host must durably reserve ID before dispatch and authenticate Actor.
type ArtifactExportRequest struct {
	SchemaVersion       int                  `json:"schema_version"`
	ID                  id.ID                `json:"id"`
	Result              ResultContentRequest `json:"result"`
	Artifact            evidence.Ref         `json:"artifact"`
	Actor               string               `json:"actor"`
	ApprovalRef         string               `json:"approval_ref"`
	Destination         string               `json:"destination"`
	DestinationRevision string               `json:"destination_revision"`
}

// ArtifactExportAuthorizer checks current actor, exact approval, source grants,
// destination permission and destination revision on every attempt, including retries.
type ArtifactExportAuthorizer interface {
	AuthorizeArtifactExport(context.Context, ArtifactExportRequest) error
}

// ArtifactExportFence holds grant, approval, destination, policy, evidence and run
// revision guards through the synchronous callback. All relevant writers must
// participate, including deletion and other processes. Remote services require
// conditional operations or a qualified observation window; there is no default.
type ArtifactExportFence interface {
	WithArtifactExportFence(context.Context, ArtifactExportRequest, Binding, func(Binding) error) error
}

// ArtifactExportScreenRequest contains private bytes and trusted identity.
type ArtifactExportScreenRequest struct {
	Request ArtifactExportRequest
	Binding Binding
	Data    []byte
}

// ArtifactExportScreenResult approves only the exact digest; rewriting is not export.
type ArtifactExportScreenResult struct {
	Approved bool
	SHA256   string
}

// ArtifactExportScreener applies current content policy to the complete bytes.
// Binary content requires a capable media-specific checker. Errors deny export.
type ArtifactExportScreener interface {
	ScreenArtifactExport(context.Context, ArtifactExportScreenRequest) (ArtifactExportScreenResult, error)
}

// ArtifactExportReceipt identifies a durable delivery, never a reusable grant.
type ArtifactExportReceipt struct {
	SchemaVersion int    `json:"schema_version"`
	ID            id.ID  `json:"id"`
	RequestDigest string `json:"request_digest"`
	SHA256        string `json:"sha256"`
	Bytes         int64  `json:"bytes"`
}

// ArtifactExportSink durably binds an operation ID to its full request before
// writing. Identical retries must verify the same delivery; changed requests
// must conflict across destinations. It must enforce destination path policy,
// honor cancellation and retain recovery evidence. No non-idempotent fallback.
type ArtifactExportSink interface {
	ExportArtifact(context.Context, ArtifactExportRequest, []byte) (ArtifactExportReceipt, error)
}

// ArtifactExporterConfig requires explicit current authority and an idempotent sink.
// MaxConcurrent bounds admission from one to 64. All ports must honor cancellation.
type ArtifactExporterConfig struct {
	Store         ResultContentStore
	Repository    evidence.Repository
	Authorizer    ArtifactExportAuthorizer
	Fence         ArtifactExportFence
	Screener      ArtifactExportScreener
	Sink          ArtifactExportSink
	Clock         Clock
	MaxConcurrent int
}

// ArtifactExporter is a separate host command; sandbox execution cannot invoke it.
type ArtifactExporter struct {
	config ArtifactExporterConfig
	slots  chan struct{}
}

// Freeze validates and detaches one exact export request.
func (r ArtifactExportRequest) Freeze() (ArtifactExportRequest, string, error) {
	if r.SchemaVersion != 1 || r.ID.IsNil() || r.ID.Prefix() != PrefixExport || r.Result.Validate() != nil || r.Artifact.Validate() != nil || r.Artifact.Bytes > 1<<20 || !evaluationName(r.Actor, 512) || !evaluationName(r.ApprovalRef, 512) || !evaluationName(r.Destination, 512) || !evaluationName(r.DestinationRevision, 256) {
		return ArtifactExportRequest{}, "", ErrInvalid
	}
	digest, err := recordDigest(r)
	if err != nil {
		return ArtifactExportRequest{}, "", err
	}
	body, err := json.Marshal(r)
	if err != nil {
		return ArtifactExportRequest{}, "", ErrInvalid
	}
	var frozen ArtifactExportRequest
	if json.Unmarshal(body, &frozen) != nil {
		return ArtifactExportRequest{}, "", ErrInvalid
	}
	return frozen, digest, nil
}

// Validate binds a delivery receipt to its exact request.
func (r ArtifactExportReceipt) Validate(request ArtifactExportRequest) error {
	_, digest, err := request.Freeze()
	if err != nil || r.SchemaVersion != 1 || r.ID != request.ID || r.RequestDigest != digest || r.SHA256 != request.Artifact.SHA256 || r.Bytes != request.Artifact.Bytes {
		return ErrInvalid
	}
	return nil
}

// NewArtifactExporter refuses missing authority, screening, storage or delivery.
func NewArtifactExporter(c ArtifactExporterConfig) (*ArtifactExporter, error) {
	if c.MaxConcurrent < 1 || c.MaxConcurrent > 64 || nilHostPort(c.Store) || nilHostPort(c.Repository) || nilHostPort(c.Authorizer) || nilHostPort(c.Fence) || nilHostPort(c.Screener) || nilHostPort(c.Sink) || nilHostPort(c.Clock) {
		return nil, ErrInvalid
	}
	return &ArtifactExporter{config: c, slots: make(chan struct{}, c.MaxConcurrent)}, nil
}

type artifactExportFence struct {
	fence   ArtifactExportFence
	request ArtifactExportRequest
}

func (f artifactExportFence) WithCommitFence(ctx context.Context, b Binding, next func(Binding) error) error {
	detached, _, err := f.request.Freeze()
	if err != nil {
		return err
	}
	return f.fence.WithArtifactExportFence(ctx, detached, b, next)
}

// Export rechecks approval, result freshness and exact screened bytes under the
// export fence. Any failure after delivery starts is uncertain, even cancellation.
// Retry only this same request against the sink's durable operation reservation.
func (e *ArtifactExporter) Export(ctx context.Context, request ArtifactExportRequest) (ArtifactExportReceipt, error) {
	if e == nil || e.slots == nil {
		return ArtifactExportReceipt{}, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return ArtifactExportReceipt{}, err
	}
	r, _, err := request.Freeze()
	if err != nil {
		return ArtifactExportReceipt{}, err
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return ArtifactExportReceipt{}, err
	}
	if scope.Canonical() != r.Artifact.Scope.Canonical() {
		return ArtifactExportReceipt{}, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, MaxArtifactExportDuration)
	defer cancel()
	select {
	case e.slots <- struct{}{}:
		defer func() { <-e.slots }()
	default:
		return ArtifactExportReceipt{}, cortex.ErrVerificationUnavailable
	}
	if checkErr := e.authorize(ctx, r); checkErr != nil {
		return ArtifactExportReceipt{}, checkErr
	}
	committed, err := e.config.Store.GetCommittedResult(ctx, r.Result.ResultID)
	if err != nil {
		return ArtifactExportReceipt{}, resultReadError(err)
	}
	if committed.Validate() != nil || committed.Request.ResultID != r.Result.ResultID || committed.Request.Current.RunID != r.Result.RunID || committed.Request.Current.Scope.Canonical() != scope.Canonical() {
		return ArtifactExportReceipt{}, ErrConflict
	}
	binding, err := cloneBinding(committed.Request.Current)
	if err != nil {
		return ArtifactExportReceipt{}, ErrInvalid
	}
	expected, err := binding.Digest()
	if err != nil {
		return ArtifactExportReceipt{}, ErrInvalid
	}
	read := ResultReadRequest{SchemaVersion: 1, RunID: r.Result.RunID, ResultID: r.Result.ResultID, ReceiptDigest: committed.Digest}
	var attempted atomic.Bool
	receipt, err := withBindingFence(ctx, artifactExportFence{fence: e.config.Fence, request: r}, binding, func(fencedCtx context.Context, current Binding) (ArtifactExportReceipt, error) {
		digest, checkErr := current.Digest()
		if checkErr != nil || digest != expected {
			return ArtifactExportReceipt{}, ErrConflict
		}
		snapshot, readErr := readCurrentResultSnapshot(fencedCtx, e.config.Store, e.config.Clock, read)
		if readErr != nil {
			return ArtifactExportReceipt{}, readErr
		}
		if !bindingHasArtifact(snapshot.Receipt.Request.Current, r.Artifact) {
			return ArtifactExportReceipt{}, ErrInvalid
		}
		if authErr := e.authorize(fencedCtx, r); authErr != nil {
			return ArtifactExportReceipt{}, authErr
		}
		data, loadErr := e.readArtifact(fencedCtx, r)
		if loadErr != nil {
			return ArtifactExportReceipt{}, loadErr
		}
		screenRequest, _, freezeErr := r.Freeze()
		if freezeErr != nil {
			return ArtifactExportReceipt{}, freezeErr
		}
		screenBinding, cloneErr := cloneBinding(binding)
		if cloneErr != nil {
			return ArtifactExportReceipt{}, cloneErr
		}
		screened, screenErr := e.config.Screener.ScreenArtifactExport(fencedCtx, ArtifactExportScreenRequest{Request: screenRequest, Binding: screenBinding, Data: bytes.Clone(data)})
		if screenErr != nil {
			return ArtifactExportReceipt{}, resultReadError(screenErr)
		}
		if !screened.Approved || screened.SHA256 != r.Artifact.SHA256 {
			return ArtifactExportReceipt{}, safety.ErrBlocked
		}
		if _, readErr = readCurrentResultSnapshot(fencedCtx, e.config.Store, e.config.Clock, read); readErr != nil {
			return ArtifactExportReceipt{}, readErr
		}
		if authErr := e.authorize(fencedCtx, r); authErr != nil {
			return ArtifactExportReceipt{}, authErr
		}
		delivery, _, freezeErr := r.Freeze()
		if freezeErr != nil {
			return ArtifactExportReceipt{}, freezeErr
		}
		if contextErr := resultReadContextError(fencedCtx); contextErr != nil {
			return ArtifactExportReceipt{}, contextErr
		}
		attempted.Store(true)
		delivered, deliveryErr := e.config.Sink.ExportArtifact(fencedCtx, delivery, bytes.Clone(data))
		if deliveryErr != nil {
			return ArtifactExportReceipt{}, resultReadError(deliveryErr)
		}
		if validationErr := delivered.Validate(r); validationErr != nil {
			return ArtifactExportReceipt{}, validationErr
		}
		if _, readErr = readCurrentResultSnapshot(fencedCtx, e.config.Store, e.config.Clock, read); readErr != nil {
			return ArtifactExportReceipt{}, readErr
		}
		if authErr := e.authorize(fencedCtx, r); authErr != nil {
			return ArtifactExportReceipt{}, authErr
		}
		return delivered, nil
	})
	if err != nil {
		if attempted.Load() {
			return ArtifactExportReceipt{}, errors.Join(ErrExportUncertain, resultReadError(err))
		}
		return ArtifactExportReceipt{}, resultReadError(err)
	}
	return receipt, nil
}

func (e *ArtifactExporter) authorize(ctx context.Context, r ArtifactExportRequest) error {
	if err := resultReadContextError(ctx); err != nil {
		return err
	}
	now := e.config.Clock.Now()
	if now.IsZero() || r.Artifact.CreatedAt.After(now) || !r.Artifact.RetainUntil.After(now) {
		return ErrInvalid
	}
	detached, _, err := r.Freeze()
	if err != nil {
		return err
	}
	if authErr := e.config.Authorizer.AuthorizeArtifactExport(ctx, detached); authErr != nil {
		if errors.Is(authErr, context.Canceled) || errors.Is(authErr, context.DeadlineExceeded) {
			return resultReadError(authErr)
		}
		return ErrResultAccessDenied
	}
	return resultReadContextError(ctx)
}
func (e *ArtifactExporter) readArtifact(ctx context.Context, r ArtifactExportRequest) (data []byte, err error) {
	detached, _, err := r.Freeze()
	if err != nil {
		return nil, err
	}
	reader, err := e.config.Repository.Open(ctx, detached.Artifact)
	if reader != nil {
		defer func() { err = errors.Join(err, reader.Close()) }()
	}
	if err != nil {
		return nil, err
	}
	if reader == nil {
		return nil, ErrInvalid
	}
	data, err = io.ReadAll(io.LimitReader(reader, r.Artifact.Bytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != r.Artifact.Bytes || evidence.Digest(data) != r.Artifact.SHA256 {
		return nil, evidence.ErrDigestMismatch
	}
	return data, resultReadContextError(ctx)
}
func bindingHasArtifact(b Binding, ref evidence.Ref) bool {
	expected, err := ref.BindingDigest()
	if err != nil {
		return false
	}
	for _, known := range append([]evidence.Ref{b.Candidate}, b.Evidence...) {
		actual, digestErr := known.BindingDigest()
		if digestErr == nil && actual == expected {
			return true
		}
	}
	return false
}
