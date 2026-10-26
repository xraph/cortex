package mongo

import (
	"testing"

	"github.com/xraph/cortex/store/storetest"
)

func TestCurrentTaskAcceptanceRead(t *testing.T) {
	storetest.CurrentTaskAcceptanceRead(t, newConformanceStore(t, true))
}
