package execution

import (
	"encoding/json"
	"math"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

const PrefixReservation id.Prefix = "bres"

const (
	MaxReservations     = 1024
	MaxReservationDepth = 16
)

var (
	ErrInvalidReservation  = cortex.ErrInvalidReservation
	ErrReservationConflict = cortex.ErrReservationConflict
	ErrReservationNotFound = cortex.ErrReservationNotFound
	ErrBudgetExceeded      = cortex.ErrSharedBudgetExceeded
)

// Resources uses explicit nonnegative allowances. Zero permits no consumption,
// unlike the optional zero limits in a single-run Budget. Money is USD micros.
type Resources struct {
	Steps     int64 `json:"steps"`
	Tokens    int64 `json:"tokens"`
	ToolCalls int64 `json:"tool_calls"`
	FanOut    int64 `json:"fan_out"`
	Replans   int64 `json:"replans"`
	USDMicros int64 `json:"usd_micros"`
}

func (r Resources) values() [6]int64 {
	return [6]int64{r.Steps, r.Tokens, r.ToolCalls, r.FanOut, r.Replans, r.USDMicros}
}
func resources(v [6]int64) Resources { return Resources{v[0], v[1], v[2], v[3], v[4], v[5]} }

// Validate refuses negative amounts.
func (r Resources) Validate() error {
	for _, v := range r.values() {
		if v < 0 {
			return ErrInvalidReservation
		}
	}
	return nil
}

// Add conservatively saturates aggregate overflow. Individual receipts retain
// their original amounts; Overflow prohibits admission instead of wrapping.
func (r Resources) Add(other Resources) (Resources, bool) {
	a, b := r.values(), other.values()
	overflow := false
	for i := range a {
		if a[i] < 0 || b[i] < 0 || a[i] > math.MaxInt64-b[i] {
			a[i] = math.MaxInt64
			overflow = true
		} else {
			a[i] += b[i]
		}
	}
	return resources(a), overflow
}

// Covers tests every dimension without arithmetic that can overflow.
func (r Resources) Covers(other Resources) bool {
	if r.Validate() != nil || other.Validate() != nil {
		return false
	}
	a, b := r.values(), other.values()
	for i := range a {
		if b[i] > a[i] {
			return false
		}
	}
	return true
}

// Max retains the larger charge in each dimension.
func (r Resources) Max(other Resources) Resources {
	a, b := r.values(), other.values()
	for i := range a {
		a[i] = max(a[i], b[i])
	}
	return resources(a)
}

// Admit checks a new allocation against charged capacity without overflow.
func (r Resources) Admit(used, want Resources) error {
	if r.Validate() != nil || used.Validate() != nil || want.Validate() != nil {
		return ErrInvalidReservation
	}
	limits, a, b := r.values(), used.values(), want.values()
	for i := range limits {
		if a[i] > limits[i] || b[i] > limits[i]-a[i] {
			return ErrBudgetExceeded
		}
	}
	return nil
}

// ReservationPolicy fixes one shared account for a plan's lifetime. TokenOnly
// explicitly disables a money ceiling. Monetary bounds require host price data.
type ReservationPolicy struct {
	SchemaVersion   int          `json:"schema_version"`
	PlanID          id.ID        `json:"plan_id"`
	Scope           cortex.Scope `json:"scope"`
	Limits          Resources    `json:"limits"`
	Deadline        time.Time    `json:"deadline"`
	TokenOnly       bool         `json:"token_only"`
	PricingRevision string       `json:"pricing_revision,omitempty"`
}

// Freeze validates and detaches the immutable account policy.
func (p ReservationPolicy) Freeze() (ReservationPolicy, string, error) {
	if p.SchemaVersion != 1 || p.PlanID.IsNil() || cortex.ValidateScope(p.Scope) != nil || p.Limits.Validate() != nil || p.Limits.FanOut < 1 || p.Limits.FanOut > MaxReservations || p.Deadline.IsZero() {
		return ReservationPolicy{}, "", ErrInvalidReservation
	}
	if p.TokenOnly {
		if p.Limits.USDMicros != 0 || p.PricingRevision != "" {
			return ReservationPolicy{}, "", ErrInvalidReservation
		}
	} else if p.Limits.USDMicros <= 0 || !operationText(p.PricingRevision, 256) {
		return ReservationPolicy{}, "", ErrInvalidReservation
	}
	data, err := json.Marshal(p)
	if err != nil || len(data) > 16<<10 {
		return ReservationPolicy{}, "", ErrInvalidReservation
	}
	var frozen ReservationPolicy
	if json.Unmarshal(data, &frozen) != nil {
		return ReservationPolicy{}, "", ErrInvalidReservation
	}
	return frozen, evidence.Digest(data), nil
}

// Admit checks deadline, policy pins and conservative aggregate capacity.
func (p ReservationPolicy) Admit(used Resources, r ReservationRequest, at time.Time) error {
	if _, _, err := p.Freeze(); err != nil {
		return err
	}
	if _, _, err := r.Freeze(); err != nil {
		return err
	}
	if at.IsZero() || !at.Before(p.Deadline) || !at.Before(r.Deadline) || r.Deadline.After(p.Deadline) || r.PlanID != p.PlanID || r.Scope.Canonical() != p.Scope.Canonical() {
		return ErrInvalidReservation
	}
	if !p.moneyMatches(r) {
		return ErrInvalidReservation
	}
	return p.Limits.Admit(used, r.Allocation)
}

// ReservationKind distinguishes an executable leaf from delegated capacity.
type ReservationKind string

const (
	ReservationLeaf   ReservationKind = "leaf"
	ReservationBranch ReservationKind = "branch"
)

// ReservationRequest binds capacity to a task and host-owned attempt reference.
// Branches allocate to children and cannot report their own execution usage.
type ReservationRequest struct {
	SchemaVersion   int             `json:"schema_version"`
	ID              id.ID           `json:"id"`
	PlanID          id.ID           `json:"plan_id"`
	PlanDigest      string          `json:"plan_digest"`
	Scope           cortex.Scope    `json:"scope"`
	TaskID          id.ID           `json:"task_id"`
	ParentID        id.ID           `json:"parent_id,omitempty"`
	OwnerRef        string          `json:"owner_ref"`
	Kind            ReservationKind `json:"kind"`
	Allocation      Resources       `json:"allocation"`
	Deadline        time.Time       `json:"deadline"`
	MoneyBound      bool            `json:"money_bound"`
	PricingRevision string          `json:"pricing_revision,omitempty"`
}

// Freeze validates immutable reservation identity and bounded allocation.
func (r ReservationRequest) Freeze() (ReservationRequest, string, error) {
	if r.SchemaVersion != 1 || r.ID.IsNil() || r.ID.Prefix() != PrefixReservation || r.PlanID.IsNil() || r.TaskID.IsNil() || !evidence.ValidDigest(r.PlanDigest) || cortex.ValidateScope(r.Scope) != nil || (!r.ParentID.IsNil() && (r.ParentID.Prefix() != PrefixReservation || r.ParentID == r.ID)) || !operationText(r.OwnerRef, 512) || r.Allocation.Validate() != nil || r.Allocation.FanOut < 1 || r.Allocation.FanOut > MaxReservations || r.Deadline.IsZero() {
		return ReservationRequest{}, "", ErrInvalidReservation
	}
	if (r.Kind == ReservationLeaf && r.Allocation.FanOut != 1) || (r.Kind == ReservationBranch && r.Allocation.FanOut < 2) || (r.Kind != ReservationLeaf && r.Kind != ReservationBranch) {
		return ReservationRequest{}, "", ErrInvalidReservation
	}
	if r.MoneyBound {
		if !operationText(r.PricingRevision, 256) {
			return ReservationRequest{}, "", ErrInvalidReservation
		}
	} else if r.PricingRevision != "" || r.Allocation.USDMicros != 0 {
		return ReservationRequest{}, "", ErrInvalidReservation
	}
	data, err := json.Marshal(r)
	if err != nil || len(data) > 16<<10 {
		return ReservationRequest{}, "", ErrInvalidReservation
	}
	var frozen ReservationRequest
	if json.Unmarshal(data, &frozen) != nil {
		return ReservationRequest{}, "", ErrInvalidReservation
	}
	return frozen, evidence.Digest(data), nil
}

// KnownResources marks dimensions with a trusted usage receipt.
type KnownResources uint8

const (
	KnownSteps KnownResources = 1 << iota
	KnownTokens
	KnownToolCalls
	KnownFanOut
	KnownReplans
	KnownMoney
	KnownAll = KnownSteps | KnownTokens | KnownToolCalls | KnownFanOut | KnownReplans | KnownMoney
)

// ReservationUsage distinguishes known zero from missing usage.
type ReservationUsage struct {
	Actual Resources      `json:"actual"`
	Known  KnownResources `json:"known"`
}

// Settlement is final host-attributed usage. Unknown dimensions retain their
// allocation or the attributed minimum, whichever is larger. A branch closes
// only from its stored child charges, not this usage.
type Settlement struct {
	ReservationID id.ID            `json:"reservation_id"`
	RequestDigest string           `json:"request_digest"`
	CloseBranch   bool             `json:"close_branch,omitempty"`
	Usage         ReservationUsage `json:"usage"`
	Receipt       string           `json:"receipt,omitempty"`
	// Minimum retains observed lower bounds when usage is incomplete.
	Minimum *Resources `json:"minimum,omitempty"`
	// Overflow marks unknown dimensions whose observed sum exceeded int64.
	Overflow KnownResources `json:"overflow,omitempty"`
}

// Freeze validates and fingerprints a settlement without trusting its receipt.
func (s Settlement) Freeze() (Settlement, string, error) {
	if s.ReservationID.IsNil() || s.ReservationID.Prefix() != PrefixReservation || !evidence.ValidDigest(s.RequestDigest) || s.Usage.Actual.Validate() != nil || s.Usage.Known & ^KnownAll != 0 || len(s.Receipt) > 4096 {
		return Settlement{}, "", ErrInvalidReservation
	}
	if s.Overflow & ^KnownAll != 0 || s.Overflow&s.Usage.Known != 0 {
		return Settlement{}, "", ErrInvalidReservation
	}
	var minimum [6]int64
	if s.Minimum != nil {
		if s.Minimum.Validate() != nil {
			return Settlement{}, "", ErrInvalidReservation
		}
		copyMinimum := *s.Minimum
		s.Minimum = &copyMinimum
		minimum = copyMinimum.values()
	}
	if s.CloseBranch {
		if s.Usage != (ReservationUsage{}) || s.Receipt != "" || s.Minimum != nil || s.Overflow != 0 {
			return Settlement{}, "", ErrInvalidReservation
		}
	} else {
		if (s.Usage.Known != 0 || s.Minimum != nil || s.Overflow != 0) && !operationText(s.Receipt, 4096) {
			return Settlement{}, "", ErrInvalidReservation
		}
		for i, v := range s.Usage.Actual.values() {
			if s.Usage.Known&(1<<i) == 0 && v != 0 {
				return Settlement{}, "", ErrInvalidReservation
			}
			if (s.Usage.Known&(1<<i) != 0 && v < minimum[i]) || (s.Overflow&(1<<i) != 0 && minimum[i] != math.MaxInt64) {
				return Settlement{}, "", ErrInvalidReservation
			}
		}
	}
	data, err := json.Marshal(s)
	if err != nil {
		return Settlement{}, "", ErrInvalidReservation
	}
	return s, evidence.Digest(data), nil
}

// SettlementCharge refunds only known unused leaf capacity. Each reservation
// consumes at least one fan-out slot, including an attempt that never dispatched.
func SettlementCharge(r ReservationRequest, s Settlement) (Resources, error) {
	_, digest, err := r.Freeze()
	if err != nil {
		return Resources{}, err
	}
	if _, _, err = s.Freeze(); err != nil {
		return Resources{}, err
	}
	if s.CloseBranch || r.Kind != ReservationLeaf || s.ReservationID != r.ID || s.RequestDigest != digest {
		return Resources{}, ErrInvalidReservation
	}
	charge, actual := r.Allocation.values(), s.Usage.Actual.values()
	if s.Minimum != nil {
		charge = r.Allocation.Max(*s.Minimum).values()
	}
	for i := range charge {
		if s.Usage.Known&(1<<i) != 0 {
			charge[i] = actual[i]
		}
	}
	charge[3] = max(int64(1), charge[3])
	if !r.MoneyBound && charge[5] != 0 {
		return Resources{}, ErrInvalidReservation
	}
	return resources(charge), nil
}

func (p ReservationPolicy) moneyMatches(r ReservationRequest) bool {
	if p.TokenOnly {
		return !r.MoneyBound && r.PricingRevision == "" && r.Allocation.USDMicros == 0
	}
	return r.MoneyBound && r.PricingRevision == p.PricingRevision
}
