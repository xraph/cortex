package sqlite

import (
	"testing"

	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/storetest"
)

func TestReflectionExecution(t *testing.T) {
	storetest.ReflectionExecution(t, func(t *testing.T) store.Store { return newTestStore(t) })
}
