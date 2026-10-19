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
	instr      *engineMetrics
	checker    safety.SegmentChecker
	pending    string
	approved   strings.Builder
	tokenIndex int
}

func (e *Engine) newSegmentPublication(ctx context.Context, ag *agent.Config, r *run.Run, p safety.Policy) (seg *segmentPublication, openErr error) {
	if p.Mode != safety.ModeCheckedSegments {
		return nil, nil
	}
	defer func() {
		if openErr != nil {
			e.instr.publicationChecked(segmentSource, safety.DirectionOutput, publicationErrorLabel(openErr))
		}
	}()
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
	return &segmentPublication{checker: checker, instr: e.instr}, nil
}

// segmentSource is the publication source label of a streaming segment
// check. Segment checks are counted but get no span: there is one per
// decided stretch of a streamed answer, and the step span already covers
// them.
const segmentSource = "model_output_segment"

func (s *segmentPublication) add(ctx context.Context, r *run.Run, p safety.Policy, text string, final bool, events chan<- StreamEvent) (addErr error) {
	// A call that only buffers (nothing consumed, not final) decided
	// nothing and is not counted.
	decided := false
	status := safety.StatusUnscanned
	defer func() {
		if decided || addErr != nil {
			s.instr.publicationChecked(segmentSource, safety.DirectionOutput, publicationResult(status, addErr))
		}
	}()
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
	decided = true
	switch decision.Status {
	case safety.StatusApproved, safety.StatusRedacted, safety.StatusFlagged:
		status = decision.Status
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
