package sqlite

import (
	"testing"

	"github.com/xraph/cortex/store/storetest"
)

func TestCoordinationEvents(t *testing.T) { storetest.CoordinationEvents(t, newTestStore(t)) }
