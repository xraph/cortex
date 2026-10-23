package event

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// EncodeCursor produces a canonical, versioned token without JSON number rounding.
// Tokens bind a position to a run; they do not grant access to that run.
func EncodeCursor(cursor Cursor) (string, error) {
	if cursor.Validate() != nil {
		return "", ErrInvalid
	}
	return "1." + cursor.RunID.String() + "." + strconv.FormatUint(cursor.Sequence, 10), nil
}

// DecodeCursor rejects foreign runs and noncanonical tokens. An empty token starts
// at zero, which may produce an explicit retention gap on the first read.
func DecodeCursor(runID id.AgentRunID, token string) (Cursor, error) {
	cursor := Cursor{SchemaVersion: 1, RunID: runID}
	if cursor.Validate() != nil || len(token) > 128 {
		return Cursor{}, ErrInvalid
	}
	if token == "" {
		return cursor, nil
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "1" || parts[1] != runID.String() {
		return Cursor{}, ErrInvalid
	}
	seq, err := strconv.ParseUint(parts[2], 10, 64)
	if err != nil || strconv.FormatUint(seq, 10) != parts[2] {
		return Cursor{}, ErrInvalid
	}
	cursor.Sequence = seq
	if cursor.Validate() != nil {
		return Cursor{}, ErrInvalid
	}
	return cursor, nil
}

// ReplayPage is the shared transport envelope. A gap requires the consumer to
// refresh its run snapshot before continuing from NextCursor.
type ReplayPage struct {
	Page       Page   `json:"page"`
	NextCursor string `json:"next_cursor"`
	Gap        bool   `json:"gap"`
}

// ReadReplayPage reads one validated metadata page through an authorized reader.
// Pass an engine configured with WithPublicEventReplay or an equivalent adapter
// around Reader.Read, never a raw store. Zero limit selects MaxPageSize.
func ReadReplayPage(ctx context.Context, authorized ReadStore, runID id.AgentRunID, token string, limit int) (ReplayPage, error) {
	request, err := replayReadRequest(runID, token, limit)
	if err != nil {
		return ReplayPage{}, err
	}
	if nilPort(authorized) {
		return ReplayPage{}, ErrUnavailable
	}
	page, err := authorized.ReadPublicEvents(ctx, request)
	gap := errors.Is(err, ErrCursorGap)
	if err != nil && !gap {
		return ReplayPage{}, err
	}
	if page.Validate(request, gap) != nil {
		return ReplayPage{}, ErrUnavailable
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return ReplayPage{}, contextErr
	}
	next, err := EncodeCursor(page.Next)
	if err != nil {
		return ReplayPage{}, ErrUnavailable
	}
	return ReplayPage{Page: page, NextCursor: next, Gap: gap}, nil
}

func replayReadRequest(runID id.AgentRunID, token string, limit int) (ReadRequest, error) {
	cursor, err := DecodeCursor(runID, token)
	if err != nil {
		return ReadRequest{}, err
	}
	if limit == 0 {
		limit = MaxPageSize
	}
	request := ReadRequest{After: cursor, Limit: limit}
	if validationErr := request.Validate(); validationErr != nil {
		return ReadRequest{}, validationErr
	}
	return request, nil
}

// ReplayFailure contains static transport-safe diagnostics, never provider causes.
type ReplayFailure struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// DescribeReplayFailure classifies a failure without exposing internal error text.
func DescribeReplayFailure(err error) ReplayFailure {
	switch {
	case errors.Is(err, ErrAccessDenied), errors.Is(err, cortex.ErrNoScope), errors.Is(err, cortex.ErrInvalidScope), errors.Is(err, cortex.ErrUnsupportedScopeDepth):
		return ReplayFailure{Code: "replay_denied", Message: "Public event replay is not permitted"}
	case errors.Is(err, ErrUnavailable):
		return ReplayFailure{Code: "replay_unavailable", Message: "Public event replay is unavailable", Retryable: true}
	case errors.Is(err, ErrInvalid):
		return ReplayFailure{Code: "replay_invalid", Message: "Invalid public event replay request"}
	case errors.Is(err, cortex.ErrRunNotFound):
		return ReplayFailure{Code: "replay_not_found", Message: "Run was not found in your scope"}
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return ReplayFailure{Code: "replay_interrupted", Message: "Public event replay was interrupted", Retryable: true}
	default:
		return ReplayFailure{Code: "replay_unavailable", Message: "Public event replay is unavailable", Retryable: true}
	}
}
