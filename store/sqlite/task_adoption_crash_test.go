package sqlite

import (
	"testing"

	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
	"github.com/xraph/cortex/store/storetest"
)

func TestTaskAdoptionProcessCrash(t *testing.T) {
	storetest.TaskAdoptionProcessCrash(t, func(t *testing.T, path string) (store.Store, assurance.Driver) {
		s := openTestStore(t, path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
		return s, assuranceDriver{store: s}
	})
}
