package sqlite

import (
	"testing"

	"github.com/xraph/cortex/store/storetest"
)

func TestEffectContinuationExecution(t *testing.T) {
	s := newTestStore(t)
	storetest.EffectContinuationExecution(t, s, assuranceDriver{store: s})
}
