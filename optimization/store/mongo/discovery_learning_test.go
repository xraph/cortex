package mongo

import (
	"testing"

	"github.com/xraph/cortex/optimization/store/storetest"
)

func TestDiscoveryLearning(t *testing.T) { storetest.DiscoveryLearning(t, conformanceFactory(t)) }
func TestDiscoveryLearningRecovery(t *testing.T) {
	testRecovery(t, storetest.DiscoveryLearningRecovery)
}
