package state

import "github.com/xraph/cortex/optimization"

// BeginUsageCall commits the debit and one dispatch marker together. Callers may
// invoke the provider only after the surrounding transaction commits successfully.
// Repeating an identical call is an unknown attempt, never dispatch permission.
func (tx *transaction) BeginUsageCall(request optimization.UsageReservation, digest string) (optimization.Reservation, error) {
	var empty optimization.Reservation
	experiment, err := tx.reservationExperiment(request)
	if err != nil {
		return empty, err
	}
	call := optimization.UsageCall{RequestDigest: digest, StartedAt: tx.now, Lease: request.Lease}
	if validationErr := call.Validate(); validationErr != nil {
		return empty, validationErr
	}
	if prior, exists := tx.data.Reservations[experiment.ID.String()][request.CallKey]; exists && (prior.Call != nil || prior.State != optimization.ReservationHeld) {
		if !sameReservationRequest(prior.Request, request) || prior.Call != nil && prior.Call.RequestDigest != digest {
			return empty, optimization.ErrConflict
		}
		return empty, optimization.ErrUnknownAttempt
	}
	// A different call may have reported an overrun since this reservation was
	// created. Existing reservations do not exempt dispatch from that stop.
	if budgetErr := tx.checkFamilyBudget(experiment, 0, 0); budgetErr != nil {
		return empty, budgetErr
	}
	reserved, err := tx.Reserve(request)
	if err != nil {
		return empty, err
	}
	call.Lease = experiment.Lease
	reserved.Call = &call
	reserved.Revision++
	tx.data.Reservations[experiment.ID.String()][request.CallKey] = reserved
	if err := tx.appendUsageEvent(experiment, "usage_call_started", reserved, request.Lease.Owner); err != nil {
		return empty, err
	}
	return clone(reserved)
}
