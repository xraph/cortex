package shield

import (
	"context"
	"strings"
	"sync"

	"github.com/xraph/cortex/safety"

	"github.com/xraph/shield/scan"
)

// defaultSegmentLimit is how many bytes a segment checker holds before it
// cuts a run with no sentence or line end at its last whitespace.
const defaultSegmentLimit = 2048

// NewSegmentChecker opens a checker for one stream of model output, so the
// engine can publish shield-approved text as it arrives
// (safety.ModeCheckedSegments). req is the stream's output request: its
// profile, app, agent and run apply to every segment, and its content is
// ignored. The tenant comes from the scope on ctx, as for every scan, and
// the checker refuses to open without one.
//
// Each check approves a prefix of the pending text, cut where a word cannot
// continue: after the last line break or the last sentence end (".", "!",
// "?" or ":" followed by a space or a line break). With neither, the
// pending text is held until it reaches the segment limit (see
// WithSegmentLimit), and is then cut after its last whitespace; a single
// word is never split, so a secret is always scanned whole. A final check
// takes everything left.
//
// The prefix is scanned with ScanOutput exactly as Check scans model
// output, and its decision maps as safety.DecisionFromScan says. A scan
// that cannot run fails closed with safety.ErrUnavailable, a blocked
// segment returns safety.ErrBlocked, and either one ends the stream: every
// later check fails the same way, so text held back is never published.
// Errors never carry the pending text.
func (a *Adapter) NewSegmentChecker(ctx context.Context, req safety.PublicationRequest) (safety.SegmentChecker, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a == nil || a.eng == nil || req.Direction != safety.DirectionOutput {
		return nil, safety.ErrUnavailable
	}
	if _, err := a.scanContext(ctx, &req.ScanRequest); err != nil {
		return nil, err
	}
	req.Content = ""
	return &segmentChecker{adapter: a, req: req, limit: a.segmentLimit}, nil
}

// segmentChecker checks one stream's model output a segment at a time. It
// is safe for concurrent use, though the engine calls it from one
// goroutine.
type segmentChecker struct {
	adapter *Adapter
	req     safety.PublicationRequest
	limit   int

	mu sync.Mutex
	// err is the error that ended the stream, a block or a failed scan.
	err error
	// settled is set by the final check.
	settled bool
}

// CheckSegment approves the next segment of pending, as NewSegmentChecker
// describes. It returns the decision for that segment and its length in
// bytes, which is zero while the checker holds.
func (c *segmentChecker) CheckSegment(ctx context.Context, pending string, final bool) (safety.PublicationDecision, int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return safety.PublicationDecision{}, 0, c.err
	}
	if c.settled {
		return safety.PublicationDecision{}, 0, safety.ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		c.err = err
		return safety.PublicationDecision{}, 0, err
	}

	n := len(pending)
	if !final {
		n = segmentEnd(pending, c.limit)
		if n == 0 {
			return safety.PublicationDecision{}, 0, nil
		}
	} else if n == 0 {
		// Nothing is left to publish, so there is nothing to scan.
		c.settled = true
		return safety.PublicationDecision{Status: safety.StatusApproved}, 0, nil
	}

	segment := c.req
	segment.Content = pending[:n]
	result, err := c.adapter.scan(ctx, &segment.ScanRequest, scan.DirectionOutput, segment.Source, false)
	var decision safety.PublicationDecision
	if err == nil {
		decision, err = safety.DecisionFromScan(segment, result)
	}
	if err != nil {
		c.err = err
		return safety.PublicationDecision{}, 0, err
	}
	c.settled = final
	return decision, n, nil
}

// segmentEnd returns how many bytes of pending a non-final check takes: up
// to and including the last line break or sentence end with its following
// space, else, once pending reaches limit, up to and including the last
// whitespace, else zero. Every cut falls after an ASCII byte, so it is on a
// UTF-8 boundary.
func segmentEnd(pending string, limit int) int {
	for i := len(pending) - 1; i >= 0; i-- {
		switch pending[i] {
		case '\n', '\r':
			return i + 1
		case ' ':
			if i > 0 && strings.IndexByte(".!?:", pending[i-1]) >= 0 {
				return i + 1
			}
		}
	}
	if len(pending) < limit {
		return 0
	}
	return strings.LastIndexAny(pending, " \t\n\r\v\f") + 1
}
