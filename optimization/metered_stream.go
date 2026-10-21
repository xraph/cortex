package optimization

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/xraph/cortex/llm"
)

type meteredStream struct {
	mu       sync.Mutex
	call     *modelCall
	source   llm.Stream
	finished bool
	finalErr error
	closeErr error
	usage    *llm.Usage
	bytes    int
}

func (s *meteredStream) Next(ctx context.Context) (*llm.Chunk, error) {
	if err := s.call.client.checkScope(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return nil, s.finalErr
	}
	if err := ctx.Err(); err != nil {
		s.finish(err, false)
		return nil, s.finalErr
	}
	operation, cancel := context.WithCancel(s.call.ctx)
	if deadline, ok := ctx.Deadline(); ok {
		cancel()
		operation, cancel = context.WithDeadline(s.call.ctx, deadline)
	}
	stop := context.AfterFunc(ctx, cancel)
	if err := operation.Err(); err != nil {
		stop()
		cancel()
		s.finish(err, false)
		return nil, s.finalErr
	}
	chunk, err := s.source.Next(operation)
	completed := errors.Is(err, io.EOF)
	err = withoutEOF(err)
	contextErr := operation.Err()
	contextErr = errors.Join(contextErr, ctx.Err())
	stop()
	cancel()
	if contextErr != nil {
		err = errors.Join(err, contextErr)
	}
	var detached *llm.Chunk
	if chunk != nil {
		encoded, encodeErr := json.Marshal(chunk)
		if encodeErr != nil || len(encoded) > s.call.client.config.MaxResponseBytes-s.bytes {
			err = errors.Join(err, ErrInvalidSpec)
		} else {
			s.bytes += len(encoded)
			var copyErr error
			detached, copyErr = copyModelJSON(chunk, s.call.client.config.MaxResponseBytes)
			err = errors.Join(err, copyErr)
		}
	} else if err == nil && !completed {
		err = ErrIncompleteEvidence
	}
	if err != nil || completed {
		s.finish(err, completed && err == nil)
		return detached, s.finalErr
	}
	return detached, nil
}

func (s *meteredStream) Close() error {
	s.call.cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.finished {
		s.finish(context.Canceled, false)
	}
	return s.closeErr
}

func (s *meteredStream) Usage() *llm.Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyModelUsage(s.usage)
}

func (s *meteredStream) finish(cause error, completed bool) {
	s.finished = true
	s.call.cancel()
	var providerCloseErr error
	s.usage, providerCloseErr = closeModelStream(s.source)
	accountErr := s.call.settle(s.usage, "", completed && providerCloseErr == nil, errors.Join(cause, providerCloseErr))
	accountErr = nonIterationError(accountErr)
	s.closeErr = errors.Join(providerCloseErr, accountErr)
	if completed && cause == nil && s.closeErr == nil {
		s.finalErr = io.EOF
	} else {
		s.finalErr = errors.Join(cause, s.closeErr)
	}
}

// withoutEOF removes the iteration sentinel while retaining other causes. The
// engine checks errors.Is(EOF) first, so failed accounting must never wrap EOF.
func withoutEOF(err error) error {
	// Match only the sentinel here; joined EOF can carry a failure to preserve.
	if err == nil || err == io.EOF { //nolint:errorlint // Recursive unwrapping below preserves non-EOF causes.
		return nil
	}
	if !errors.Is(err, io.EOF) {
		return err
	}
	if group, ok := err.(interface{ Unwrap() []error }); ok {
		var causes []error
		for _, cause := range group.Unwrap() {
			causes = append(causes, withoutEOF(cause))
		}
		return errors.Join(causes...)
	}
	if cause := errors.Unwrap(err); cause != nil {
		return withoutEOF(cause)
	}
	return fmt.Errorf("%w: stream error: %s", ErrIncompleteEvidence, err.Error())
}

func closeModelStream(source llm.Stream) (*llm.Usage, error) {
	before := copyModelUsage(source.Usage())
	err := nonIterationError(source.Close())
	after := copyModelUsage(source.Usage())
	beforeTokens, beforeErr := observedModelTokens(before)
	afterTokens, afterErr := observedModelTokens(after)
	if beforeTokens > afterTokens || after == nil {
		after = before
	}
	return after, errors.Join(err, beforeErr, afterErr)
}

func nonIterationError(err error) error {
	if errors.Is(err, io.EOF) {
		return errors.Join(ErrIncompleteEvidence, withoutEOF(err))
	}
	return err
}
