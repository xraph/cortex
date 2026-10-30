package shield

import (
	"context"
	"strings"
	"sync"

	"github.com/xraph/cortex/safety"

	"github.com/xraph/shield/scan"
)

// defaultSegmentLimit is how many bytes a segment checker holds before it
// cuts a run with no sentence or line end at a whitespace.
const defaultSegmentLimit = 2048

// NewSegmentChecker opens a checker for one stream of model output, so the
// engine can publish shield-approved text as it arrives
// (safety.ModeCheckedSegments). req is the stream's output request: its
// profile, app, agent and run apply to every segment, and its content is
// ignored. The tenant comes from the scope on ctx, as for every scan, and
// the checker refuses to open without one.
//
// Each check approves a prefix of the pending text: through the last line
// break, or through the last sentence end (".", "!" or "?" followed by a
// space or a line break). A colon is not a sentence end, so a line such as
// "password: value" reaches shield in one scan. With neither, the pending
// text is held until it reaches the segment limit (see WithSegmentLimit),
// and is then cut after its last whitespace that does not sit next to a
// ':' or '=' (a key and its value stay together); a single word is never
// split. A PEM block is held from the start of its "-----BEGIN " line until
// its matching "-----END " line arrives, so shield sees the whole block in
// one scan; a block that never closes is held until the final check, and
// the engine's output limit bounds the hold. A final check takes everything
// left.
//
// Shield still sees each segment on its own. A rule that needs text from
// more than one segment does not see it, and approved text is already
// published, so a host that can't accept any partial text streamed under
// these rules should use buffered publication.
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

// segmentEnd returns how many bytes of pending a non-final check takes:
// up to and including the last line break or sentence end with its
// following space, else, once pending reaches limit, up to and including
// the last whitespace that does not split an assignment, else zero. The
// cut is then lowered to hold any PEM block it would open without closing
// (see pemHold). Every cut falls after an ASCII byte or at a line start, so
// it is on a UTF-8 boundary.
func segmentEnd(pending string, limit int) int {
	cut := boundaryEnd(pending)
	if cut == 0 && len(pending) >= limit {
		cut = limitEnd(pending)
	}
	return pemHold(pending, cut)
}

// boundaryEnd returns the end of the last line break or sentence end in
// pending, or zero.
func boundaryEnd(pending string) int {
	for i := len(pending) - 1; i >= 0; i-- {
		switch pending[i] {
		case '\n', '\r':
			return i + 1
		case ' ':
			if i > 0 && strings.IndexByte(".!?", pending[i-1]) >= 0 {
				return i + 1
			}
		}
	}
	return 0
}

// limitEnd returns the end of the last whitespace in pending that does not
// sit between a key and its value, or zero. Shield finds an assignment as
// a key, blanks, ':' or '=' (or ":=", "=>"), blanks, then the value, so a
// cut in the blanks on either side of the operator would scan the key and
// the value apart. Blanks at the very end of pending are not a cut either,
// since the operator after them may be in the next chunk.
func limitEnd(pending string) int {
	for i := len(pending) - 1; i >= 0; i-- {
		if strings.IndexByte(" \t\n\r\v\f", pending[i]) < 0 {
			continue
		}
		before, after := i, i
		for before >= 0 && isBlank(pending[before]) {
			before--
		}
		for after < len(pending) && isBlank(pending[after]) {
			after++
		}
		// Blanks that run to the end of pending may sit before an
		// operator that hasn't arrived yet.
		if after == len(pending) {
			continue
		}
		if before >= 0 && strings.IndexByte(":=>", pending[before]) >= 0 {
			continue
		}
		if after < len(pending) && strings.IndexByte(":=", pending[after]) >= 0 {
			continue
		}
		return i + 1
	}
	return 0
}

// isBlank reports whether b is a space or a tab, the blanks shield allows
// around an assignment's operator.
func isBlank(b byte) bool { return b == ' ' || b == '\t' }

// pemBegin and pemEnd open and close a PEM block's armor lines.
const (
	pemBegin = "-----BEGIN "
	pemEnd   = "-----END "
)

// pemHold lowers cut so it never falls inside a PEM block: when
// pending[:cut] holds a "-----BEGIN " whose matching "-----END <label>-----"
// is not also in pending[:cut], the cut moves back to the start of that
// BEGIN line. Blocks are matched in order, so a closed block before an
// open one can still go. A BEGIN line whose label is not complete yet is
// closed by any END marker.
func pemHold(pending string, cut int) int {
	from := 0
	for {
		b := strings.Index(pending[from:cut], pemBegin)
		if b < 0 {
			return cut
		}
		b += from
		marker := pemEnd
		labelStart := b + len(pemBegin)
		if n := strings.Index(pending[labelStart:cut], "-----"); n >= 0 && !strings.ContainsAny(pending[labelStart:labelStart+n], "\r\n") {
			marker = pemEnd + pending[labelStart:labelStart+n] + "-----"
		}
		e := strings.Index(pending[labelStart:cut], marker)
		if e < 0 {
			return strings.LastIndexAny(pending[:b], "\r\n") + 1
		}
		from = labelStart + e + len(marker)
	}
}
