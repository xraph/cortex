package execution

import (
	"time"

	"github.com/xraph/cortex/id"
)

type reservationAccount struct {
	byID        map[id.ID]Reservation
	children    map[id.ID][]id.ID
	childCharge map[id.ID]Resources
	charge      map[id.ID]Resources
	overflow    bool
	total       Resources
}

func accountReservations(p ReservationPolicy, reservations []Reservation) (*reservationAccount, error) {
	if _, _, err := p.Freeze(); err != nil {
		return nil, err
	}
	if len(reservations) > MaxReservations {
		return nil, ErrInvalidReservation
	}
	a := &reservationAccount{byID: make(map[id.ID]Reservation, len(reservations)), children: make(map[id.ID][]id.ID), childCharge: make(map[id.ID]Resources), charge: make(map[id.ID]Resources)}
	for _, r := range reservations {
		if r.Validate() != nil || !p.moneyMatches(r.Request) || r.Request.PlanID != p.PlanID || r.Request.Scope.Canonical() != p.Scope.Canonical() {
			return nil, ErrInvalidReservation
		}
		if _, exists := a.byID[r.Request.ID]; exists {
			return nil, ErrInvalidReservation
		}
		a.byID[r.Request.ID] = r
		a.children[r.Request.ParentID] = append(a.children[r.Request.ParentID], r.Request.ID)
	}
	for _, r := range reservations {
		if !r.Request.ParentID.IsNil() {
			parent, ok := a.byID[r.Request.ParentID]
			if !ok || parent.Request.Kind != ReservationBranch || r.Request.Deadline.After(parent.Request.Deadline) || r.CreatedAt.Before(parent.CreatedAt) {
				return nil, ErrInvalidReservation
			}
		}
	}
	visiting := make(map[id.ID]bool)
	var charge func(id.ID, int) (Resources, error)
	charge = func(identifier id.ID, depth int) (Resources, error) {
		if depth > MaxReservationDepth || visiting[identifier] {
			return Resources{}, ErrInvalidReservation
		}
		if c, ok := a.charge[identifier]; ok {
			return c, nil
		}
		visiting[identifier] = true
		r := a.byID[identifier]
		var result Resources
		if r.Request.Kind == ReservationLeaf {
			if len(a.children[identifier]) != 0 {
				return Resources{}, ErrInvalidReservation
			}
			result = r.Request.Allocation
			if r.Settlement != nil {
				var err error
				result, err = SettlementCharge(r.Request, r.Settlement.Request)
				if err != nil {
					return Resources{}, err
				}
			}
		} else {
			used := Resources{FanOut: 1}
			for _, child := range a.children[identifier] {
				childCharge, err := charge(child, depth+1)
				if err != nil {
					return Resources{}, err
				}
				var overflow bool
				used, overflow = used.Add(childCharge)
				a.overflow = a.overflow || overflow
				if r.Settlement != nil && (a.byID[child].Settlement == nil || a.byID[child].Settlement.At.After(r.Settlement.At)) {
					return Resources{}, ErrReservationConflict
				}
			}
			a.childCharge[identifier] = used
			if r.Settlement == nil {
				result = r.Request.Allocation.Max(used)
			} else {
				if !r.Settlement.Request.CloseBranch {
					return Resources{}, ErrInvalidReservation
				}
				result = used
			}
		}
		visiting[identifier] = false
		a.charge[identifier] = result
		return result, nil
	}
	for _, root := range a.children[id.ID{}] {
		c, err := charge(root, 1)
		if err != nil {
			return nil, err
		}
		var overflow bool
		a.total, overflow = a.total.Add(c)
		a.overflow = a.overflow || overflow
	}
	if len(a.charge) != len(reservations) {
		return nil, ErrInvalidReservation
	}
	return a, nil
}

// AccountReservations charges each branch once while propagating child overruns
// immediately to its ancestors. Closed branches refund only unallocated or known
// unused capacity; every retained reservation consumes one fan-out slot.
func AccountReservations(p ReservationPolicy, reservations []Reservation) (BudgetSnapshot, error) {
	a, err := accountReservations(p, reservations)
	if err != nil {
		return BudgetSnapshot{}, err
	}
	return BudgetSnapshot{Policy: p, Charged: a.total, Overflow: a.overflow, Reservations: len(reservations)}, nil
}

// AdmitReservation checks both the root account and the immediate parent before
// admitting a new allocation. Account inputs must come from trusted storage.
func AdmitReservation(p ReservationPolicy, reservations []Reservation, r ReservationRequest, at time.Time) error {
	if err := p.Admit(Resources{}, r, at); err != nil {
		return err
	}
	a, err := accountReservations(p, reservations)
	if err != nil {
		return err
	}
	if a.overflow || len(reservations) >= MaxReservations {
		return ErrBudgetExceeded
	}
	if _, exists := a.byID[r.ID]; exists {
		return ErrReservationConflict
	}
	if !r.ParentID.IsNil() {
		parent, exists := a.byID[r.ParentID]
		if !exists || parent.Request.Kind != ReservationBranch || parent.Settlement != nil || r.Deadline.After(parent.Request.Deadline) {
			return ErrReservationConflict
		}
		if admitErr := parent.Request.Allocation.Admit(a.childCharge[r.ParentID], r.Allocation); admitErr != nil {
			return admitErr
		}
	}
	_, digest, err := r.Freeze()
	if err != nil {
		return err
	}
	next := append(append([]Reservation(nil), reservations...), Reservation{Request: r, RequestDigest: digest, CreatedAt: at})
	prospective, err := accountReservations(p, next)
	if err != nil {
		return err
	}
	if prospective.overflow || !p.Limits.Covers(prospective.total) {
		return ErrBudgetExceeded
	}
	return nil
}
