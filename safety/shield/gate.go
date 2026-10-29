package shield

import (
	"context"
	"strings"

	"github.com/xraph/cortex/safety"

	"github.com/xraph/shield/scan"
)

// Check screens content for publication. It scans in the request's
// direction under the request's profile, telling shield where the text came
// from. Inbound text from an untrusted source (see WithUntrustedSources) is
// marked untrusted, so unless shield blocks it, shield's quoted text is what
// gets published, with status redacted. The outbound pass over the same
// text is not marked again, which would quote it twice.
//
// Quoting is a fixed point. Inbound untrusted text that is already exactly
// one quote for its source, the way shield writes it, is not wrapped again:
// its inner text is scanned as untrusted, and when shield changes nothing
// the same bytes come back. The engine relies on this when it re-checks a
// prepared run's knowledge before every pass. Anything else, a forged or
// partial wrapper included, is quoted as usual, and shield escapes the tags
// inside it. An untrusted result that is not one quote is refused.
//
// The result maps as safety.DecisionFromScan says. A scan that cannot run
// fails closed with safety.ErrUnavailable.
func (a *Adapter) Check(ctx context.Context, req safety.PublicationRequest) (safety.PublicationDecision, error) {
	var direction scan.Direction
	switch req.Direction {
	case safety.DirectionInput:
		direction = scan.DirectionInput
	case safety.DirectionOutput:
		direction = scan.DirectionOutput
	default:
		return safety.PublicationDecision{}, safety.ErrUnavailable
	}
	untrusted := a != nil && direction == scan.DirectionInput && a.untrusted[req.Source]
	if !untrusted {
		result, err := a.scan(ctx, &req.ScanRequest, direction, req.Source, false)
		if err != nil {
			return safety.PublicationDecision{}, err
		}
		return safety.DecisionFromScan(req, result)
	}

	scanned := req
	if inner, ok := unquote(req.Source, req.Content); ok {
		scanned.Content = inner
	}
	result, err := a.scan(ctx, &scanned.ScanRequest, direction, req.Source, true)
	if err != nil {
		return safety.PublicationDecision{}, err
	}
	decision, err := safety.DecisionFromScan(scanned, result)
	if err != nil {
		return safety.PublicationDecision{}, err
	}
	if _, ok := unquote(req.Source, decision.Text); !ok {
		return safety.PublicationDecision{}, safety.ErrUnavailable
	}
	decision.Status = safety.StatusRedacted
	return decision, nil
}

// unquote returns the inner text of text when text is exactly one quote for
// source as scan.Quote writes it: quoting the inner text again gives back
// text byte for byte. A wrapper that closes early, has text around it,
// names another source or holds an unescaped tag is not one quote.
func unquote(source, text string) (string, bool) {
	empty := scan.Quote(source, "")
	open := empty[:strings.IndexByte(empty, '\n')+1]
	closing := empty[len(open):]
	if len(text) < len(open)+len(closing) || !strings.HasPrefix(text, open) || !strings.HasSuffix(text, closing) {
		return "", false
	}
	inner := text[len(open) : len(text)-len(closing)]
	if scan.Quote(source, inner) != text {
		return "", false
	}
	return inner, true
}
