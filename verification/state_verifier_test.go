package verification_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/verification"
)

type stateObserver struct {
	calls   int
	observe func(verification.StateProbeRequest) (verification.StateObservation, error)
}

func (o *stateObserver) ObserveState(_ context.Context, r verification.StateProbeRequest) (verification.StateObservation, error) {
	o.calls++
	return o.observe(r)
}

func stateFixture(t *testing.T) (verification.StateVerifierConfig, verification.StateRequest, *stateObserver, *evaluationAuthority) {
	t.Helper()
	_, req, _, _, authority := sandboxEvaluationFixture(t)
	req.Criterion.Kind = verification.KindState
	req.Criterion.MaxAge = time.Minute
	req.Criterion.MaxSourceAge = 30 * time.Second
	now := time.Now().UTC()
	req.Binding.Context.Sources = []verification.Source{{Resource: "inventory:one", Revision: "v1", Digest: evidence.Digest([]byte("captured state")), RetrievedAt: now.Add(-time.Second)}}
	m, err := verification.SealStateManifest(verification.StateManifest{SchemaVersion: 1, Binding: req.Binding, Criterion: req.Criterion, Probes: req.Binding.Context.Sources, MaxObservationWindow: time.Second})
	require.NoError(t, err)
	observer := &stateObserver{observe: func(r verification.StateProbeRequest) (verification.StateObservation, error) {
		digest, e := r.Binding.Digest()
		require.NoError(t, e)
		source := r.Source
		source.RetrievedAt = now
		return verification.StateObservation{BindingDigest: digest, ManifestDigest: r.ManifestDigest, Source: source, StartedAt: now, FinishedAt: now}, nil
	}}
	return verification.StateVerifierConfig{Manifests: []verification.StateManifest{m}, Observer: observer, Authorizer: authority, Clock: evaluationClock{now}, MaxConcurrent: 1}, verification.StateRequest(req), observer, authority
}

func TestStateVerifierReadsRegisteredResource(t *testing.T) {
	cfg, req, observer, _ := stateFixture(t)
	v, err := verification.NewStateVerifier(cfg)
	require.NoError(t, err)
	result, err := v.CheckState(cortex.WithScope(t.Context(), req.Binding.Scope), req)
	require.NoError(t, err)
	require.Equal(t, verification.StatusPassed, result.Status)
	require.Equal(t, 1, observer.calls)
	require.Len(t, result.StateObservations, 1)
	require.Equal(t, cfg.Clock.Now().Add(30*time.Second), result.StateObservations[0].ExpiresAt)
}

func TestStateVerifierRejectsWrongTenantAndRequirement(t *testing.T) {
	for _, change := range []string{"tenant", "requirement", "criterion"} {
		t.Run(change, func(t *testing.T) {
			cfg, req, observer, _ := stateFixture(t)
			v, err := verification.NewStateVerifier(cfg)
			require.NoError(t, err)
			switch change {
			case "tenant":
				req.Binding.Scope.Levels[0].Value = "other"
			case "requirement":
				req.Binding.ContractDigest = evidence.Digest([]byte("irrelevant objective"))
			case "criterion":
				req.Criterion.ID = "other"
			}
			_, err = v.CheckState(cortex.WithScope(t.Context(), req.Binding.Scope), req)
			require.Error(t, err)
			require.Zero(t, observer.calls)
		})
	}
}

func TestStateVerifierChangedSourceFails(t *testing.T) {
	cfg, req, observer, _ := stateFixture(t)
	original := observer.observe
	observer.observe = func(r verification.StateProbeRequest) (verification.StateObservation, error) {
		o, err := original(r)
		o.Source.Revision = "v2"
		return o, err
	}
	v, err := verification.NewStateVerifier(cfg)
	require.NoError(t, err)
	result, err := v.CheckState(cortex.WithScope(t.Context(), req.Binding.Scope), req)
	require.NoError(t, err)
	require.Equal(t, verification.StatusFailed, result.Status)
	require.Len(t, result.StateObservations, 1)
}

func TestStateVerifierRejectsMalformedOrReplayedObservation(t *testing.T) {
	for _, change := range []string{"binding", "manifest", "resource", "future", "replay", "wide window", "inverted window", "source time", "supplied expiry"} {
		t.Run(change, func(t *testing.T) {
			cfg, req, observer, _ := stateFixture(t)
			original := observer.observe
			observer.observe = func(r verification.StateProbeRequest) (verification.StateObservation, error) {
				o, err := original(r)
				switch change {
				case "binding":
					o.BindingDigest = evidence.Digest([]byte("other"))
				case "manifest":
					o.ManifestDigest = evidence.Digest([]byte("other"))
				case "resource":
					o.Source.Resource = "other"
				case "future":
					o.FinishedAt = o.FinishedAt.Add(time.Second)
				case "replay":
					o.StartedAt = o.StartedAt.Add(-time.Minute)
					o.FinishedAt = o.StartedAt
					o.Source.RetrievedAt = o.StartedAt
				case "wide window":
					o.StartedAt = o.StartedAt.Add(-2 * time.Second)
				case "inverted window":
					o.FinishedAt = o.StartedAt.Add(-time.Second)
				case "source time":
					o.Source.RetrievedAt = o.FinishedAt.Add(time.Second)
				case "supplied expiry":
					o.ExpiresAt = o.FinishedAt.Add(time.Hour)
				}
				return o, err
			}
			v, err := verification.NewStateVerifier(cfg)
			require.NoError(t, err)
			_, err = v.CheckState(cortex.WithScope(t.Context(), req.Binding.Scope), req)
			require.Error(t, err)
		})
	}
}

func TestStateVerifierRevocationAndObserverFailure(t *testing.T) {
	for _, mode := range []string{"revoked", "outage"} {
		t.Run(mode, func(t *testing.T) {
			cfg, req, observer, authority := stateFixture(t)
			original := observer.observe
			observer.observe = func(r verification.StateProbeRequest) (verification.StateObservation, error) {
				o, err := original(r)
				if mode == "revoked" {
					authority.denied = true
				} else {
					err = errors.New("source unavailable")
				}
				return o, err
			}
			v, err := verification.NewStateVerifier(cfg)
			require.NoError(t, err)
			result, err := v.CheckState(cortex.WithScope(t.Context(), req.Binding.Scope), req)
			require.Error(t, err)
			require.NotEqual(t, verification.StatusPassed, result.Status)
		})
	}
}

func TestStateVerifierReportRetainsAndExpiresObservationWindow(t *testing.T) {
	cfg, req, _, _ := stateFixture(t)
	contract, _, _ := fixture(t)
	contract.Criteria = []verification.Criterion{req.Criterion}
	var err error
	contract, err = verification.SealContract(contract)
	require.NoError(t, err)
	req.Binding.ContractDigest = contract.Digest
	cfg.Manifests[0].Binding = req.Binding
	cfg.Manifests[0], err = verification.SealStateManifest(cfg.Manifests[0])
	require.NoError(t, err)
	checker, err := verification.NewStateVerifier(cfg)
	require.NoError(t, err)
	host, err := verification.NewVerifier([]verification.Registration{verification.RegisterState(req.Criterion.Verifier, req.Criterion.VerifierRevision, req.Criterion.Parameters.ManifestRevision, checker)}, cfg.Authorizer, cfg.Clock)
	require.NoError(t, err)
	report, err := host.Verify(cortex.WithScope(t.Context(), req.Binding.Scope), verification.Request{Contract: contract, Binding: req.Binding, Deadline: req.Deadline, Verifier: req.Criterion.Verifier, VerifierRevision: req.Criterion.VerifierRevision})
	require.NoError(t, err)
	require.Len(t, report.Outcomes[0].StateObservations, 1)
	require.NoError(t, report.Validate())
	require.Equal(t, verification.StatusPassed, verification.CheckFreshness(report, req.Binding, cfg.Clock.Now()))
	require.Equal(t, verification.StatusStale, verification.CheckFreshness(report, req.Binding, cfg.Clock.Now().Add(30*time.Second)))
	report.Outcomes[0].StateObservations[0].Source.Revision = "tampered state"
	_, err = verification.SealReport(report)
	require.ErrorIs(t, err, verification.ErrInvalid)
}

func TestStateManifestRejectsUncapturedOrUnboundedProbes(t *testing.T) {
	for _, mode := range []string{"schema", "kind", "no probes", "unknown probe", "duplicate", "unbounded age", "unbounded window", "oversized window"} {
		t.Run(mode, func(t *testing.T) {
			cfg, _, _, _ := stateFixture(t)
			m := cfg.Manifests[0]
			switch mode {
			case "schema":
				m.SchemaVersion = 2
			case "kind":
				m.Criterion.Kind = verification.KindTest
			case "no probes":
				m.Probes = nil
			case "unknown probe":
				m.Probes = append([]verification.Source(nil), m.Probes...)
				m.Probes[0].Resource = "other"
			case "duplicate":
				m.Probes = append(m.Probes, m.Probes[0])
			case "unbounded age":
				m.Criterion.MaxSourceAge = 0
			case "unbounded window":
				m.MaxObservationWindow = 0
			case "oversized window":
				m.MaxObservationWindow = 31 * time.Second
			}
			_, err := verification.SealStateManifest(m)
			require.ErrorIs(t, err, verification.ErrInvalid)
		})
	}
}

func TestStateVerifierRequiresPortsAndUnambiguousRegistration(t *testing.T) {
	for _, mode := range []string{"observer", "typed nil", "authority", "clock", "empty", "duplicate", "changed manifest", "unbounded admission"} {
		t.Run(mode, func(t *testing.T) {
			cfg, _, _, _ := stateFixture(t)
			switch mode {
			case "observer":
				cfg.Observer = nil
			case "typed nil":
				cfg.Observer = (*stateObserver)(nil)
			case "authority":
				cfg.Authorizer = nil
			case "clock":
				cfg.Clock = nil
			case "empty":
				cfg.Manifests = nil
			case "duplicate":
				cfg.Manifests = append(cfg.Manifests, cfg.Manifests[0])
			case "changed manifest":
				cfg.Manifests[0].MaxObservationWindow = time.Millisecond
			case "unbounded admission":
				cfg.MaxConcurrent = 0
			}
			_, err := verification.NewStateVerifier(cfg)
			require.ErrorIs(t, err, verification.ErrInvalid)
		})
	}
}

func TestStateVerifierFreezesRegistrationAndCallbackArguments(t *testing.T) {
	cfg, req, observer, _ := stateFixture(t)
	original := observer.observe
	observer.observe = func(r verification.StateProbeRequest) (verification.StateObservation, error) {
		o, err := original(r)
		r.Binding.Scope.Levels[0].Value = "changed callback tenant"
		r.Binding.Context.Sources[0].Revision = "changed callback source"
		return o, err
	}
	v, err := verification.NewStateVerifier(cfg)
	require.NoError(t, err)
	// The caller and registration share slices, so detach the request before
	// mutating the registration to exercise both boundaries independently.
	req.Binding.Scope.Levels = append([]cortex.Level(nil), req.Binding.Scope.Levels...)
	req.Binding.Context.Sources = append([]verification.Source(nil), req.Binding.Context.Sources...)
	cfg.Manifests[0].Probes[0].Revision = "changed registration"
	for i := 0; i < 2; i++ {
		result, checkErr := v.CheckState(cortex.WithScope(t.Context(), req.Binding.Scope), req)
		require.NoError(t, checkErr)
		require.Equal(t, verification.StatusPassed, result.Status)
	}
}

func TestStateVerifierBoundsAdmissionAndCancelledReads(t *testing.T) {
	cfg, req, observer, _ := stateFixture(t)
	v, err := verification.NewStateVerifier(cfg)
	require.NoError(t, err)
	original := observer.observe
	observer.observe = func(r verification.StateProbeRequest) (verification.StateObservation, error) {
		_, nestedErr := v.CheckState(cortex.WithScope(t.Context(), req.Binding.Scope), req)
		require.ErrorIs(t, nestedErr, cortex.ErrVerificationUnavailable)
		return original(r)
	}
	_, err = v.CheckState(cortex.WithScope(t.Context(), req.Binding.Scope), req)
	require.NoError(t, err)
	_, err = v.CheckState(cortex.WithScope(t.Context(), req.Binding.Scope), req)
	require.NoError(t, err)
	cancelled, cancel := context.WithCancel(cortex.WithScope(t.Context(), req.Binding.Scope))
	cancel()
	_, err = v.CheckState(cancelled, req)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 2, observer.calls)
}

type deadlineStateObserver struct {
	t       *testing.T
	window  time.Duration
	observe func(verification.StateProbeRequest) (verification.StateObservation, error)
}

func (o deadlineStateObserver) ObserveState(ctx context.Context, r verification.StateProbeRequest) (verification.StateObservation, error) {
	deadline, ok := ctx.Deadline()
	require.True(o.t, ok)
	require.LessOrEqual(o.t, time.Until(deadline), o.window)
	return o.observe(r)
}

func TestStateProbeUsesManifestDeadline(t *testing.T) {
	cfg, req, observer, _ := stateFixture(t)
	cfg.Observer = deadlineStateObserver{t: t, window: cfg.Manifests[0].MaxObservationWindow, observe: observer.observe}
	v, err := verification.NewStateVerifier(cfg)
	require.NoError(t, err)
	_, err = v.CheckState(cortex.WithScope(t.Context(), req.Binding.Scope), req)
	require.NoError(t, err)
}
