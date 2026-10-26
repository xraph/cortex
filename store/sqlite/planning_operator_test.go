package sqlite

import (
	"testing"

	"github.com/xraph/cortex/store/storetest"
)

func TestCurrentTaskAcceptanceRead(t *testing.T) {
	storetest.CurrentTaskAcceptanceRead(t, newTestStore(t))
}
