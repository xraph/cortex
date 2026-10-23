package a2aremote

import (
	"context"
	"time"
	"unicode/utf8"

	"github.com/xraph/cortex/safety"
	"github.com/xraph/cortex/verification"
)

// ResultContentExtensionURI identifies guarded committed content on GetTask.
const ResultContentExtensionURI = "https://cortex.xraph.dev/a2a/extensions/result-content/v1"

// ResultContentMetadataKey contains the screened result and its assurance metadata.
const ResultContentMetadataKey = "cortex.result"

// ResultIDHeader selects the committed result announced by metadata replay.
const ResultIDHeader = "Cortex-Result-ID"

// MaxResultDeliveryDuration bounds an entire remote content request and delivery.
const MaxResultDeliveryDuration = verification.MaxResultReadDuration + 10*time.Second

// ResultContentGateway must enforce current content grants, fresh evidence and
// publication policy. Implement it with engine.ReadResultContent, never GetRun.
// Host callbacks can read the authenticated identity with PeerFromContext.
type ResultContentGateway interface {
	ResultContentAvailable() bool
	ReadResultContent(context.Context, verification.ResultContentRequest) (verification.PublishedResult, error)
}

type resultContentRequest struct {
	active   bool
	resultID string
}

func parseResultContentRequest(cred Credentials) (resultContentRequest, error) {
	active, err := extensionRequested(cred, ResultContentExtensionURI)
	if err != nil {
		return resultContentRequest{}, resultContentError(verification.ErrInvalid)
	}
	value, present, err := replayHeader(cred, ResultIDHeader)
	if err != nil || present != active || (active && (value == "" || len(value) > 128)) {
		return resultContentRequest{}, resultContentError(verification.ErrInvalid)
	}
	replay, err := parseReplayRequest(cred)
	if err != nil {
		return resultContentRequest{}, err
	}
	if active && replay.active {
		return resultContentRequest{}, resultContentError(verification.ErrInvalid)
	}
	return resultContentRequest{active: active, resultID: value}, nil
}

// ResultContentRequested validates negotiation for transport admission checks.
func ResultContentRequested(cred Credentials) (bool, error) {
	req, err := parseResultContentRequest(cred)
	return req.active, err
}

// ResultContentActive identifies a service-produced guarded content envelope.
func ResultContentActive(task *Task) bool {
	if task == nil {
		return false
	}
	_, ok := task.Metadata[ResultContentMetadataKey].(verification.ResultContentResponse)
	return ok
}

func rejectResultContent(cred Credentials) error {
	req, err := parseResultContentRequest(cred)
	if err != nil {
		return err
	}
	if req.active {
		return ErrUnsupportedOperation("result content requires GetTask")
	}
	return nil
}

func (s *Service) getResultContentTask(ctx context.Context, cred Credentials, req GetTaskRequest, content resultContentRequest) (*Task, error) {
	if s.contentReads.Add(1) > 64 {
		s.contentReads.Add(-1)
		return nil, resultContentError(safety.ErrUnavailable)
	}
	defer s.contentReads.Add(-1)
	ctx, cancel := context.WithTimeout(ctx, verification.MaxResultReadDuration)
	defer cancel()
	ctx, peer, err := s.authenticate(ctx, cred)
	if err != nil {
		return nil, resultContentError(verification.ErrResultAccessDenied)
	}
	gw, ok := s.gw.(ResultContentGateway)
	if !s.opts.ResultContent || !ok || !gw.ResultContentAvailable() {
		return nil, resultContentError(safety.ErrUnavailable)
	}
	r, err := s.loadRun(ctx, req.ID)
	if err != nil || r == nil {
		return nil, resultContentError(verification.ErrNotFound)
	}
	owner, err := s.gw.GetAgentByName(ctx, req.Tenant)
	if err != nil || owner == nil || owner.ID != r.AgentID {
		return nil, resultContentError(verification.ErrNotFound)
	}
	request, err := verification.ParseResultContentRequest(r.ID.String(), content.resultID)
	if err != nil {
		return nil, resultContentError(err)
	}
	result, err := gw.ReadResultContent(ctx, request)
	if err != nil {
		return nil, resultContentError(err)
	}
	if result.SchemaVersion != 1 || result.RunID != request.RunID || result.ResultID != request.ResultID || result.RunRevision != r.Revision || result.VerificationStatus != verification.StatusPassed || result.CheckedAt.IsZero() || result.VerifiedAt.IsZero() {
		return nil, resultContentError(verification.ErrConflict)
	}
	if len(result.Content) > 1<<20 || !utf8.ValidString(result.Content) {
		return nil, resultContentError(safety.ErrLimit)
	}
	switch result.PublicationStatus {
	case safety.StatusApproved, safety.StatusFlagged:
		if result.ContentChanged {
			return nil, resultContentError(safety.ErrUnavailable)
		}
	case safety.StatusRedacted:
	default:
		return nil, resultContentError(safety.ErrUnavailable)
	}
	_, current, err := s.authenticate(ctx, cred)
	if err != nil || current.Node != peer.Node || current.Scope.Canonical() != peer.Scope.Canonical() {
		return nil, resultContentError(verification.ErrResultAccessDenied)
	}
	if err := ctx.Err(); err != nil {
		return nil, resultContentError(err)
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return nil, resultContentError(context.DeadlineExceeded)
	}
	// Construct only the public projection. Run output and diagnostics never enter it.
	return &Task{ID: r.ID.String(), Status: TaskStatus{State: TaskStateCompleted, Timestamp: timestamp(result.CheckedAt)},
		Metadata: map[string]any{ResultContentMetadataKey: verification.NewResultContentResponse(result)}}, nil
}

func resultContentError(err error) *Error {
	failure := verification.DescribeResultContentFailure(err)
	out := ErrInternal(failure.Message)
	switch failure.Code {
	case "result_invalid", "result_limit":
		out = ErrInvalidParams(failure.Message)
	case "result_denied", "result_blocked":
		out = ErrUnauthenticated()
	case "result_not_found":
		out = &Error{Code: CodeTaskNotFound, Message: failure.Message}
	}
	out.Data = []map[string]any{{"code": failure.Code, "retryable": failure.Retryable}}
	return out
}
