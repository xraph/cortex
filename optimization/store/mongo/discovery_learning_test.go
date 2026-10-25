package mongo

import (
	"testing"

	"github.com/xraph/cortex/optimization/store/storetest"
)

func TestDiscoveryLearning(t *testing.T) { storetest.DiscoveryLearning(t, conformanceFactory(t)) }
func TestDiscoveryLearningRecovery(t *testing.T) {
	testRecovery(t, storetest.DiscoveryLearningRecovery)
}

func TestDiscoveryInheritance(t *testing.T) { storetest.DiscoveryInheritance(t, conformanceFactory(t)) }
func TestDiscoveryInheritanceRecovery(t *testing.T) {
	testRecovery(t, storetest.DiscoveryInheritanceRecovery)
}
