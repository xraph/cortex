package shield

import (
	"context"

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
	result, err := a.scan(ctx, &req.ScanRequest, direction, req.Source, untrusted)
	if err != nil {
		return safety.PublicationDecision{}, err
	}
	return safety.DecisionFromScan(req, result)
}
