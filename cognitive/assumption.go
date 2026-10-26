package cognitive

import (
	"time"

	"github.com/xraph/cortex/evidence"
)

// AssumptionOrigin distinguishes hypotheses from authenticated observations.
type AssumptionOrigin string

const (
	AssumptionModel AssumptionOrigin = "model"
	AssumptionHost  AssumptionOrigin = "host"
)

// AssumptionState never establishes task acceptance or execution authority.
type AssumptionState string

const (
	AssumptionUnresolved AssumptionState = "unresolved"
	AssumptionSupported  AssumptionState = "supported"
	AssumptionDisputed   AssumptionState = "disputed"
)

// Assumption retains a short claim and references, not private reasoning. Only
// authenticated host observations may support or dispute a claim. The caller
// must verify that the referenced bytes actually support the stated transition.
type Assumption struct {
	Key      string           `json:"key"`
	Summary  string           `json:"summary"`
	Origin   AssumptionOrigin `json:"origin"`
	State    AssumptionState  `json:"state"`
	Evidence []evidence.Ref   `json:"evidence,omitempty"`
}

func (a Assumption) validate() error {
	if !contextText(a.Key, 128) || !contextText(a.Summary, 1024) || len(a.Evidence) > 16 {
		return ErrInvalidReflection
	}
	if a.Origin != AssumptionHost && a.Origin != AssumptionModel {
		return ErrInvalidReflection
	}
	if a.State != AssumptionUnresolved && a.State != AssumptionSupported && a.State != AssumptionDisputed {
		return ErrInvalidReflection
	}
	if a.State != AssumptionUnresolved && (a.Origin != AssumptionHost || len(a.Evidence) == 0) {
		return ErrInvalidReflection
	}
	return nil
}

// ServiceObservation records a bounded health observation with its evidence.
// Missing or expired observations remain unknown; registration is not health.
type ServiceObservation struct {
	Service    string       `json:"service"`
	Health     Health       `json:"health"`
	ObservedAt time.Time    `json:"observed_at"`
	ExpiresAt  time.Time    `json:"expires_at"`
	Evidence   evidence.Ref `json:"evidence"`
}

// CurrentHealth never extends an observation beyond its explicit lifetime.
func (s ServiceObservation) CurrentHealth(now time.Time) Health {
	if !contextText(s.Service, 256) || s.ObservedAt.IsZero() || s.ObservedAt.After(now) || !now.Before(s.ExpiresAt) || s.ExpiresAt.Sub(s.ObservedAt) > 5*time.Minute || s.Evidence.Validate() != nil || s.Evidence.CreatedAt.After(now) || !now.Before(s.Evidence.RetainUntil) || (s.Health != HealthHealthy && s.Health != HealthUnavailable) {
		return HealthUnknown
	}
	return s.Health
}
