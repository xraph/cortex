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

func TestDiscoveryReflection(t *testing.T) { storetest.DiscoveryReflection(t, conformanceFactory(t)) }
func TestDiscoveryReflectionRecovery(t *testing.T) {
	testRecovery(t, storetest.DiscoveryReflectionRecovery)
}

func TestImprovementWorker(t *testing.T) { storetest.ImprovementWorker(t, conformanceFactory(t)) }
func TestImprovementWorkerLoop(t *testing.T) {
	storetest.ImprovementWorkerLoop(t, conformanceFactory(t))
}
func TestImprovementWorkerRecovery(t *testing.T) {
	testRecovery(t, storetest.ImprovementWorkerRecovery)
}

func TestControllerDecisions(t *testing.T) {
	storetest.ControllerDecisions(t, conformanceFactory(t))
}
func TestControllerDecisionArtifacts(t *testing.T) {
	storetest.ControllerDecisionArtifacts(t, conformanceFactory(t))
}
func TestControllerDecisionRecovery(t *testing.T) {
	testRecovery(t, storetest.ControllerDecisionRecovery)
}

func TestControllerDecisionWorker(t *testing.T) {
	storetest.ControllerDecisionWorker(t, conformanceFactory(t))
}

func TestControllerAdoption(t *testing.T) { storetest.ControllerAdoption(t, conformanceFactory(t)) }
func TestControllerAdoptionRecovery(t *testing.T) {
	testRecovery(t, storetest.ControllerAdoptionRecovery)
}

func TestControllerAdoptionArtifactDenial(t *testing.T) {
	storetest.ControllerAdoptionArtifactDenial(t, conformanceFactory(t))
}
