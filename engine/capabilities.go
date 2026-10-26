package engine

import (
	"github.com/xraph/cortex/safety"
	"github.com/xraph/cortex/store"
)

// RuntimeCapabilities reports installed mechanisms, not a provider qualification
// or a guarantee that a particular execution will pass its checks.
type RuntimeCapabilities struct {
	PreparedExecution        bool        `json:"prepared_execution"`
	AtomicSourceSnapshot     bool        `json:"atomic_source_snapshot"`
	ContextualVerification   bool        `json:"contextual_verification"`
	PublicationGateInstalled bool        `json:"publication_gate_installed"`
	PublicationMode          safety.Mode `json:"publication_mode"`
	PublicationRequired      bool        `json:"publication_required"`
	IncrementalGateInstalled bool        `json:"incremental_gate_installed"`
	VerificationWorker       bool        `json:"verification_worker"`
	VerificationRecovery     bool        `json:"verification_recovery"`
	EffectContinuation       bool        `json:"effect_continuation"`
	BoundedReflection        bool        `json:"bounded_reflection"`
	GovernedContextSummary   bool        `json:"governed_context_summary"`
	CalibrationProvider      bool        `json:"calibration_provider"`
}

// RuntimeCapabilities returns immutable process capabilities. Scanner health
// and effectiveness are evaluated by the configured provider for each request.
func (e *Engine) RuntimeCapabilities() RuntimeCapabilities {
	_, incremental := e.publicationGate.(safety.IncrementalGate)
	_, snapshots := e.store.(store.PreparationSnapshotter)
	return RuntimeCapabilities{
		AtomicSourceSnapshot: e.atomicPreparation && snapshots, PreparedExecution: true,
		ContextualVerification:   e.requiredVerification != nil,
		PublicationGateInstalled: e.publicationGate != nil, PublicationMode: e.publicationPolicy.Mode,
		PublicationRequired: e.publicationPolicy.Required, IncrementalGateInstalled: incremental,
		VerificationWorker: e.verificationWorker != nil, VerificationRecovery: e.verificationRecovery != nil,
		EffectContinuation: e.effectContinuation != nil, BoundedReflection: e.reflection != nil,
		GovernedContextSummary: e.contextBudget != nil && e.contextBudget.Policy.Summary.Enabled() && e.requiredVerification != nil,
		CalibrationProvider:    e.operationalAwareness != nil && e.operationalAwareness.Calibration != nil,
	}
}
