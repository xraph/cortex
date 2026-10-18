package engine

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/safety"
)

// segmentPublication holds only the suffix the checker has not decided yet and
// approved text for persistence. A later rejection cannot retract emitted bytes.
type segmentPublication struct {
	checker    safety.SegmentChecker
	pending    string
	approved   strings.Builder
	tokenIndex int
}

func (e *Engine) newSegmentPublication(ctx context.Context, ag *agent.Config, r *run.Run, p safety.Policy) (*segmentPublication, error) {
	if p.Mode != safety.ModeCheckedSegments {
		return nil, nil
	}
	gate, ok := e.publicationGate.(safety.IncrementalGate)
	if !ok {
		return nil, safety.ErrUnavailable
	}
	checker, err := gate.NewSegmentChecker(ctx, publicationRequest(ctx, ag, r, "", "model_output", safety.DirectionOutput))
	if err != nil {
		return nil, closedPublicationError(err)
	}
	if checker == nil {
		return nil, safety.ErrUnavailable
	}
	return &segmentPublication{checker: checker}, nil
}

func (s *segmentPublication) add(ctx context.Context, r *run.Run, p safety.Policy, text string, final bool, events chan<- StreamEvent) error {
	if len(text) > p.MaxOutputBytes-len(s.pending) {
		return safety.ErrLimit
	}
	s.pending += text
	decision, consumed, err := s.checker.CheckSegment(ctx, s.pending, final)
	if err != nil {
		return closedPublicationError(err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if consumed < 0 || consumed > len(s.pending) || !utf8.ValidString(s.pending[:consumed]) || final && consumed != len(s.pending) {
		return safety.ErrUnavailable
	}
	if consumed == 0 && !final {
		if decision.Text != "" {
			return safety.ErrUnavailable
		}
		return nil
	}
	switch decision.Status {
	case safety.StatusApproved, safety.StatusRedacted, safety.StatusFlagged:
	default:
		return safety.ErrUnavailable
	}
	if len(decision.Text) > p.MaxOutputBytes-s.approved.Len() {
		return safety.ErrLimit
	}
	s.pending = s.pending[consumed:]
	s.approved.WriteString(decision.Text)
	r.Provenance.PublicationStatus = mergePublicationStatus(r.Provenance.PublicationStatus, decision.Status)
	if !publishToken(ctx, events, approvedText(decision.Text), s.tokenIndex) {
		return ctx.Err()
	}
	s.tokenIndex++
	return nil
}
