package a2aremote

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/event"
	"github.com/xraph/cortex/id"
)

// ReplayExtensionURI identifies opt-in metadata replay on GetTask and SubscribeToTask.
const ReplayExtensionURI = "https://cortex.xraph.dev/a2a/extensions/public-events/v1"

// ReplayMetadataKey contains the versioned replay envelope in Task.Metadata.
const ReplayMetadataKey = "cortex.replay"

// ReplayCursorHeader carries an opaque, run-bound resume cursor.
const ReplayCursorHeader = "Cortex-Replay-Cursor"

// ReplayLimitHeader bounds a page to at most event.MaxPageSize observations.
const ReplayLimitHeader = "Cortex-Replay-Limit"

// PublicEventGateway requires authorization before and after reading storage.
// Implement it with engine.ReadPublicEvents or event.Reader.Read, never a raw store.
// The authorizer can inspect PeerFromContext for the authenticated remote identity.
// Callbacks must honor context cancellation.
type PublicEventGateway interface {
	event.ReadStore
	PublicEventReplayAvailable() bool
}

type authenticatedPeerKey struct{}

// PeerFromContext returns the resolved peer with a detached scope. It is available
// to host replay policy after Service authenticates the inbound credentials.
func PeerFromContext(ctx context.Context) (Peer, bool) {
	peer, ok := ctx.Value(authenticatedPeerKey{}).(Peer)
	peer.Scope.Levels = append([]cortex.Level(nil), peer.Scope.Levels...)
	return peer, ok
}

// ReplayEventMetadata preserves integer precision across protobuf Struct and JSON.
type ReplayEventMetadata struct {
	event.Input
	SchemaVersion int           `json:"schema_version"`
	RunID         id.AgentRunID `json:"run_id"`
	Sequence      string        `json:"sequence"`
	RunRevision   string        `json:"run_revision"`
	ObservedAt    time.Time     `json:"observed_at"`
}

// ReplayWatermarkMetadata identifies retention and the current run snapshot.
type ReplayWatermarkMetadata struct {
	SchemaVersion int           `json:"schema_version"`
	RunID         id.AgentRunID `json:"run_id"`
	FirstSequence string        `json:"first_sequence"`
	LastSequence  string        `json:"last_sequence"`
	RunRevision   string        `json:"run_revision"`
	ResultID      id.ID         `json:"result_id,omitzero"`
}

// ReplayPageMetadata contains historical observations, never result content.
// A gap requires a snapshot refresh before resuming from NextCursor.
type ReplayPageMetadata struct {
	SchemaVersion int                     `json:"schema_version"`
	Events        []ReplayEventMetadata   `json:"events"`
	Watermark     ReplayWatermarkMetadata `json:"watermark"`
	NextCursor    string                  `json:"next_cursor"`
	Gap           bool                    `json:"gap"`
}

// ReplayActive identifies the typed envelope emitted by Service for bindings.
func ReplayActive(task *Task) bool {
	if task == nil {
		return false
	}
	_, ok := task.Metadata[ReplayMetadataKey].(ReplayPageMetadata)
	return ok
}

type replayRequest struct {
	active bool
	cursor string
	limit  int
}

func replayHeader(cred Credentials, name string) (value string, present bool, err error) {
	count := 0
	for key, values := range cred.Headers {
		if !strings.EqualFold(key, name) {
			continue
		}
		count++
		if count > 1 || len(values) != 1 {
			return "", false, replayError(event.ErrInvalid)
		}
		value = values[0]
	}
	return value, count > 0, nil
}

func parseReplayRequest(cred Credentials) (replayRequest, error) {
	var req replayRequest
	size, tokens := 0, 0
	for key, values := range cred.Headers {
		if !strings.EqualFold(key, "A2A-Extensions") {
			continue
		}
		for _, value := range values {
			size += len(value)
			if size > 4096 {
				return req, replayError(event.ErrInvalid)
			}
			for uri := range strings.SplitSeq(value, ",") {
				tokens++
				if tokens > 32 {
					return req, replayError(event.ErrInvalid)
				}
				if strings.TrimSpace(uri) == ReplayExtensionURI {
					req.active = true
				}
			}
		}
	}
	cursor, hasCursor, err := replayHeader(cred, ReplayCursorHeader)
	if err != nil {
		return req, err
	}
	limit, hasLimit, err := replayHeader(cred, ReplayLimitHeader)
	if err != nil {
		return req, err
	}
	if (hasCursor || hasLimit) && !req.active {
		return req, replayError(event.ErrInvalid)
	}
	if len(cursor) > 128 {
		return req, replayError(event.ErrInvalid)
	}
	req.cursor = cursor
	if hasLimit {
		n, parseErr := strconv.Atoi(limit)
		if parseErr != nil || n < 0 || n > event.MaxPageSize || strconv.Itoa(n) != limit {
			return req, replayError(event.ErrInvalid)
		}
		req.limit = n
	}
	return req, nil
}

func rejectReplay(cred Credentials) error {
	req, err := parseReplayRequest(cred)
	if err != nil {
		return err
	}
	if req.active {
		return ErrUnsupportedOperation("public event replay requires GetTask or SubscribeToTask")
	}
	return nil
}

type replaySnapshot struct {
	task *Task
	page event.ReplayPage
	peer Peer
}

func (s *Service) getReplayTask(ctx context.Context, cred Credentials, req GetTaskRequest, replay replayRequest) (*Task, error) {
	snapshot, err := s.readReplaySnapshot(ctx, cred, req, replay, nil)
	if err != nil {
		return nil, replayError(err)
	}
	return snapshot.task, nil
}

func (s *Service) readReplaySnapshot(ctx context.Context, cred Credentials, req GetTaskRequest, replay replayRequest, expected *Peer) (replaySnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ctx, peer, err := s.authenticate(ctx, cred)
	if err != nil {
		return replaySnapshot{}, event.ErrAccessDenied
	}
	if expected != nil && (expected.Node != peer.Node || expected.Scope.Canonical() != peer.Scope.Canonical()) {
		return replaySnapshot{}, event.ErrAccessDenied
	}
	gw, ok := s.gw.(PublicEventGateway)
	if !s.opts.PublicEventReplay || !ok || !gw.PublicEventReplayAvailable() {
		return replaySnapshot{}, event.ErrUnavailable
	}
	r, err := s.loadRun(ctx, req.ID)
	if err != nil {
		return replaySnapshot{}, cortex.ErrRunNotFound
	}
	if r.ID.Prefix() != id.PrefixAgentRun {
		return replaySnapshot{}, event.ErrUnavailable
	}
	owner, err := s.gw.GetAgentByName(ctx, req.Tenant)
	if err != nil || owner == nil || owner.ID != r.AgentID {
		return replaySnapshot{}, cortex.ErrRunNotFound
	}
	page, err := event.ReadReplayPage(ctx, gw, r.ID, replay.cursor, replay.limit)
	if err != nil {
		return replaySnapshot{}, err
	}
	if r.Revision != page.Page.Watermark.RunRevision {
		return replaySnapshot{}, event.ErrUnavailable
	}
	_, current, err := s.authenticate(ctx, cred)
	if err != nil || current.Node != peer.Node || current.Scope.Canonical() != peer.Scope.Canonical() {
		return replaySnapshot{}, event.ErrAccessDenied
	}
	if err := ctx.Err(); err != nil {
		return replaySnapshot{}, err
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return replaySnapshot{}, context.DeadlineExceeded
	}
	task := TaskFromRun(r, "")
	task.Artifacts, task.History, task.Status.Message = nil, nil, nil
	task.Metadata = map[string]any{ReplayMetadataKey: replayMetadata(page)}
	return replaySnapshot{task: &task, page: page, peer: peer}, nil
}

func replayMetadata(page event.ReplayPage) ReplayPageMetadata {
	w := page.Page.Watermark
	out := ReplayPageMetadata{
		SchemaVersion: 1, NextCursor: page.NextCursor, Gap: page.Gap,
		Watermark: ReplayWatermarkMetadata{SchemaVersion: w.SchemaVersion, RunID: w.RunID, FirstSequence: strconv.FormatUint(w.FirstSequence, 10), LastSequence: strconv.FormatUint(w.LastSequence, 10), RunRevision: strconv.FormatInt(w.RunRevision, 10), ResultID: w.ResultID},
		Events:    make([]ReplayEventMetadata, 0, len(page.Page.Events)),
	}
	for _, e := range page.Page.Events {
		out.Events = append(out.Events, ReplayEventMetadata{Input: e.Input, SchemaVersion: e.SchemaVersion, RunID: e.RunID, Sequence: strconv.FormatUint(e.Sequence, 10), RunRevision: strconv.FormatInt(e.RunRevision, 10), ObservedAt: e.ObservedAt})
	}
	return out
}

func replayError(err error) *Error {
	failure := event.DescribeReplayFailure(err)
	out := ErrInternal(failure.Message)
	switch failure.Code {
	case "replay_invalid":
		out = ErrInvalidParams(failure.Message)
	case "replay_denied":
		out = ErrUnauthenticated()
	case "replay_not_found":
		out = &Error{Code: CodeTaskNotFound, Message: failure.Message}
	}
	out.Data = []map[string]any{{"code": failure.Code, "retryable": failure.Retryable}}
	return out
}

func writeReplayHeaders(w http.ResponseWriter, result any) {
	task, ok := result.(*Task)
	if ok && ReplayActive(task) {
		w.Header().Set("A2A-Extensions", ReplayExtensionURI)
		w.Header().Set("Cache-Control", "no-store")
	}
}
