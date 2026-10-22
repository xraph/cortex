package execution

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrInvalidEffect identifies an incomplete or unsafe host effect declaration.
var ErrInvalidEffect = errors.New("cortex: invalid effect requirement")

// EffectClass is trusted handler metadata, never inferred from model text.
type EffectClass string

const (
	EffectReadOnly      EffectClass = "read_only"
	EffectIdempotent    EffectClass = "idempotent_mutation"
	EffectNonIdempotent EffectClass = "non_idempotent_mutation"
	EffectCompensatable EffectClass = "compensatable_mutation"
)

// RetryPolicy bounds attempts and backoff. A bound is not permission to retry
// an unknown effect; reconciliation and current authorization still apply.
type RetryPolicy struct {
	MaxAttempts    int           `json:"max_attempts"`
	InitialBackoff time.Duration `json:"initial_backoff_ns,omitempty"`
	MaxBackoff     time.Duration `json:"max_backoff_ns,omitempty"`
}

// EffectReference pins a separately authorized compensation handler.
type EffectReference struct {
	Tool     string `json:"tool"`
	Revision string `json:"revision"`
}

// EffectRequirement pins a host handler's declared behavior. IdempotencyRevision
// identifies its stable-operation-key contract. ReconcileRevision identifies a
// host probe for authoritative external state. Neither field executes a probe.
type EffectRequirement struct {
	Tool                string          `json:"tool"`
	Revision            string          `json:"revision"`
	Class               EffectClass     `json:"class"`
	Retry               RetryPolicy     `json:"retry"`
	IdempotencyRevision string          `json:"idempotency_revision,omitempty"`
	ReconcileRevision   string          `json:"reconcile_revision,omitempty"`
	Compensation        EffectReference `json:"compensation,omitzero"`
}

// Validate requires bounded attempts and explicit mutation/recovery contracts.
func (r EffectRequirement) Validate() error {
	if !effectText(r.Tool) || !effectText(r.Revision) {
		return ErrInvalidEffect
	}
	if r.Retry.MaxAttempts < 1 || r.Retry.MaxAttempts > 8 || r.Retry.InitialBackoff < 0 || r.Retry.MaxBackoff < r.Retry.InitialBackoff || r.Retry.MaxBackoff > time.Hour {
		return ErrInvalidEffect
	}
	if r.Retry.MaxAttempts > 1 && r.Retry.InitialBackoff == 0 {
		return ErrInvalidEffect
	}
	switch r.Class {
	case EffectReadOnly:
		if r.ReconcileRevision != "" && !effectText(r.ReconcileRevision) {
			return ErrInvalidEffect
		}
	case EffectIdempotent, EffectNonIdempotent, EffectCompensatable:
		if !effectText(r.ReconcileRevision) {
			return ErrInvalidEffect
		}
	default:
		return ErrInvalidEffect
	}
	if r.Class == EffectIdempotent {
		if !effectText(r.IdempotencyRevision) {
			return ErrInvalidEffect
		}
	} else if r.IdempotencyRevision != "" {
		return ErrInvalidEffect
	}
	if r.Class == EffectCompensatable {
		if !effectText(r.Compensation.Tool) || !effectText(r.Compensation.Revision) {
			return ErrInvalidEffect
		}
	} else if r.Compensation != (EffectReference{}) {
		return ErrInvalidEffect
	}
	return nil
}

func effectText(value string) bool {
	return utf8.ValidString(value) && strings.TrimSpace(value) != "" && !strings.ContainsRune(value, 0)
}
