package safety

import "context"

// PublicationStatus describes content screening, not factual verification.
type PublicationStatus string

const (
	StatusApproved  PublicationStatus = "approved"
	StatusRedacted  PublicationStatus = "redacted"
	StatusFlagged   PublicationStatus = "flagged"
	StatusUnscanned PublicationStatus = "unscanned"
)

// PublicationRequest carries content and trusted provenance separately.
type PublicationRequest struct {
	ScanRequest
	Source string `json:"source"`
}

// PublicationDecision contains only the text authorized for the next boundary.
// An empty redaction is intentional. An error must never return candidate text.
type PublicationDecision struct {
	Text   string
	Status PublicationStatus
}

// PublicationGate screens complete content. Implementations must be safe for
// concurrent use and honor context cancellation. The engine validates statuses.
type PublicationGate interface {
	Check(context.Context, PublicationRequest) (PublicationDecision, error)
}

// IncrementalGate explicitly advertises a boundary-aware streaming checker.
// Each stream owns its own checker. It must hold prefixes whose safety depends
// on future bytes; approving a prefix is irrevocable. Full-document policies
// must use buffered mode instead.
type IncrementalGate interface {
	PublicationGate
	NewSegmentChecker(context.Context, PublicationRequest) (SegmentChecker, error)
}

// SegmentChecker approves a prefix of the supplied pending text. Consumed is a
// byte count on a UTF-8 boundary. Zero holds the entire pending text. A final
// check must consume all pending bytes. The engine bounds pending and total text.
type SegmentChecker interface {
	CheckSegment(ctx context.Context, pending string, final bool) (decision PublicationDecision, consumed int, err error)
}

// ScannerGate adapts the existing input/output scanner with closed error handling.
// Scanner errors are deliberately not wrapped: providers can include raw content
// or credentials in their error text.
type ScannerGate struct{ Scanner Scanner }

// Check approves, flags or redacts content using the configured scanner.
func (g ScannerGate) Check(ctx context.Context, req PublicationRequest) (PublicationDecision, error) {
	if g.Scanner == nil {
		return PublicationDecision{}, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return PublicationDecision{}, err
	}
	var result *ScanResult
	var err error
	switch req.Direction {
	case DirectionInput:
		result, err = g.Scanner.ScanInput(ctx, &req.ScanRequest)
	case DirectionOutput:
		result, err = g.Scanner.ScanOutput(ctx, &req.ScanRequest)
	default:
		return PublicationDecision{}, ErrUnavailable
	}
	if ctx.Err() != nil {
		return PublicationDecision{}, ctx.Err()
	}
	if err != nil || result == nil {
		return PublicationDecision{}, ErrUnavailable
	}
	return DecisionFromScan(req, result)
}

// DecisionFromScan turns a scan result into the decision a gate returns
// for req. A blocked result is ErrBlocked. An allow or a flag publishes the
// scanner's Redacted text, with StatusRedacted, when it set one (a scanner
// that quotes untrusted text returns the quoted text this way), and the
// request's own content otherwise. A redact publishes Redacted, even an
// empty one. A nil result or an unknown decision is ErrUnavailable, so a
// gate that cannot read the result fails closed.
func DecisionFromScan(req PublicationRequest, result *ScanResult) (PublicationDecision, error) {
	if result == nil {
		return PublicationDecision{}, ErrUnavailable
	}
	if result.Blocked || result.Decision == DecisionBlock {
		return PublicationDecision{}, ErrBlocked
	}
	switch result.Decision {
	case DecisionAllow:
		if result.Redacted != "" {
			return PublicationDecision{Text: result.Redacted, Status: StatusRedacted}, nil
		}
		return PublicationDecision{Text: req.Content, Status: StatusApproved}, nil
	case DecisionFlag:
		if result.Redacted != "" {
			return PublicationDecision{Text: result.Redacted, Status: StatusRedacted}, nil
		}
		return PublicationDecision{Text: req.Content, Status: StatusFlagged}, nil
	case DecisionRedact:
		return PublicationDecision{Text: result.Redacted, Status: StatusRedacted}, nil
	default:
		return PublicationDecision{}, ErrUnavailable
	}
}
