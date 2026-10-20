package verification_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/verification"
)

func TestRegisteredVerifierStampsHostTimeAndBindsManifest(t *testing.T) {
	c, r, now := fixture(t)
	// The real deadline remains ahead of wall time while the injected clock is deterministic.
	deadline := time.Now().Add(time.Hour)
	runner := &testChecker{}
	host, err := verification.NewVerifier([]verification.Registration{verification.RegisterTests("tests", "v1", "tests-v1", runner)}, verificationAuthorizer{}, fixedClock{now})
	require.NoError(t, err)
	ctx := cortex.WithScope(context.Background(), c.Scope)
	req := verification.Request{Contract: c, Binding: r.Binding, Deadline: deadline, Verifier: "tests", VerifierRevision: "v1"}
	report, err := host.Verify(ctx, req)
	require.NoError(t, err)
	require.Equal(t, now, report.CheckedAt)
	require.Equal(t, verification.StatusPassed, verification.Aggregate(c, []verification.Report{report}, now).Status)
	require.Equal(t, 1, runner.calls)
	req.Contract.Criteria[0].Parameters.ManifestRevision = "unregistered"
	req.Contract, err = verification.SealContract(req.Contract)
	require.NoError(t, err)
	req.Binding.ContractDigest = req.Contract.Digest
	_, err = host.Verify(ctx, req)
	require.Error(t, err)
	require.Equal(t, 1, runner.calls)
}
func TestVerifierErrorAndCancellationNeverPass(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "checker failure", true: "cancel during authorization"}[cancel], func(t *testing.T) {
			c, r, now := fixture(t)
			checker := &testChecker{err: errors.New("unavailable")}
			ctx, stop := context.WithCancel(cortex.WithScope(context.Background(), c.Scope))
			defer stop()
			var auth verification.BindingAuthorizer = verificationAuthorizer{}
			if cancel {
				auth = cancelAuthorizer{stop}
			}
			host, err := verification.NewVerifier([]verification.Registration{verification.RegisterTests("tests", "v1", "tests-v1", checker)}, auth, fixedClock{now})
			require.NoError(t, err)
			report, err := host.Verify(ctx, verification.Request{Contract: c, Binding: r.Binding, Deadline: time.Now().Add(time.Hour), Verifier: "tests", VerifierRevision: "v1"})
			require.Error(t, err)
			require.NotEqual(t, verification.StatusPassed, verification.Aggregate(c, []verification.Report{report}, now).Status)
			if cancel {
				require.Zero(t, checker.calls)
			}
		})
	}
}

type fixedClock struct{ at time.Time }

func (c fixedClock) Now() time.Time { return c.at }

type testChecker struct {
	calls int
	err   error
}

func (c *testChecker) EvaluateTests(context.Context, verification.TestRequest) (verification.CheckResult, error) {
	c.calls++
	return verification.CheckResult{Status: verification.StatusPassed}, c.err
}

type verificationAuthorizer struct{}

func (verificationAuthorizer) AuthorizeVerification(context.Context, verification.Binding) error {
	return nil
}

type cancelAuthorizer struct{ cancel context.CancelFunc }

func (a cancelAuthorizer) AuthorizeVerification(context.Context, verification.Binding) error {
	a.cancel()
	return nil
}
