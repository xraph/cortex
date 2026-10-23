package verification_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/verification"
)

type executorStoreStub struct{ verification.ExecutorStore }
type evaluationAdapterStub struct{ verification.EvaluationAdapter }

func TestExecutorRequiresEveryHostPort(t *testing.T) {
	base := verification.ExecutorConfig{Store: executorStoreStub{}, Adapter: evaluationAdapterStub{}, Authorizer: verificationAuthorizer{}, Clock: fixedClock{time.Now()}}
	for _, mutate := range []func(*verification.ExecutorConfig){
		func(c *verification.ExecutorConfig) { c.Store = nil },
		func(c *verification.ExecutorConfig) { c.Store = (*executorStoreStub)(nil) },
		func(c *verification.ExecutorConfig) { c.Adapter = nil },
		func(c *verification.ExecutorConfig) { c.Adapter = (*evaluationAdapterStub)(nil) },
		func(c *verification.ExecutorConfig) { c.Authorizer = nil },
		func(c *verification.ExecutorConfig) { c.Authorizer = (*verificationAuthorizer)(nil) },
		func(c *verification.ExecutorConfig) { c.Clock = nil },
		func(c *verification.ExecutorConfig) { c.Clock = (*fixedClock)(nil) },
	} {
		config := base
		mutate(&config)
		executor, err := verification.NewExecutor(config)
		require.ErrorIs(t, err, verification.ErrInvalid)
		require.Nil(t, executor)
	}
}

func TestExecutorRenewalRequiresRenewableStore(t *testing.T) {
	_, err := verification.NewExecutor(verification.ExecutorConfig{Store: executorStoreStub{}, Adapter: evaluationAdapterStub{}, Authorizer: verificationAuthorizer{}, Clock: fixedClock{time.Now()}, LeaseRenewal: &verification.LeaseRenewalPolicy{Duration: time.Second, Interval: time.Millisecond}})
	require.ErrorIs(t, err, verification.ErrInvalid)
}
