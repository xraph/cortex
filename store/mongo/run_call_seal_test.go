package mongo

import (
	"testing"

	"github.com/xraph/cortex/store/storetest"
)

func TestRunCallTransactionsStandaloneUnavailable(t *testing.T) {
	storetest.RunCallTransactionsUnavailable(t, newConformanceStore(t, false))
}
