//go:build linux || darwin

package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"time"

	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/sandbox"
)

// DeploymentIdentity binds qualification to one installed worker configuration.
// ProfileDigest includes the exact configuration bytes, worker pin, service
// template and outer limits. RuntimeDigest covers the complete runtime inventory.
type DeploymentIdentity struct {
	Broker        string
	ProfileDigest string
	RuntimeDigest string
}

// DeploymentQualification is a trusted host verifier's observation, not a
// worker assertion. Both its deployment identity and capabilities must match.
type DeploymentQualification struct {
	Deployment   DeploymentIdentity
	Capabilities sandbox.Capabilities
}

// QualificationVerifier verifies retained live probe evidence for this exact
// deployment and the current host, kernel and effective isolation controls.
// It must reject revoked, stale, incomplete or fixture-only evidence and honor
// cancellation. File digests alone never establish qualification. This callback
// may run concurrently and must not derive authority from the request's payload.
type QualificationVerifier func(context.Context, DeploymentIdentity) (DeploymentQualification, error)

// NewDeploymentAttestation pins the installed Linux configuration for one host
// lifetime. Each observation verifies installation before and after consulting
// current qualification evidence. Construction does not invoke the verifier or
// qualify a worker. Installation changes require new assembly and qualification.
func NewDeploymentAttestation(ctx context.Context, name string, verify QualificationVerifier) (DeploymentAttestation, error) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return nil, errUnsupported
	}
	return newDeploymentAttestation(ctx, name, verify, 0)
}

func newDeploymentAttestation(ctx context.Context, name string, verify QualificationVerifier, owner uint32) (DeploymentAttestation, error) {
	if verify == nil {
		return nil, fmt.Errorf("cortex: missing live sandbox qualification verifier")
	}
	operation, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	configuration, err := readWorkerConfig(operation, name, owner)
	if err != nil {
		return nil, err
	}
	expected := deploymentIdentity(configuration)
	check := func(ctx context.Context) error {
		current, readErr := readWorkerConfig(ctx, name, owner)
		if readErr != nil {
			return readErr
		}
		if deploymentIdentity(current) != expected {
			return broker.ErrConflict
		}
		return ctx.Err()
	}
	return func(ctx context.Context) (sandbox.Capabilities, error) {
		bounded, finish := context.WithTimeout(ctx, 30*time.Second)
		defer finish()
		if checkErr := check(bounded); checkErr != nil {
			return sandbox.Capabilities{}, checkErr
		}
		qualified, verifyErr := verify(bounded, expected)
		if operationErr := errors.Join(verifyErr, bounded.Err()); operationErr != nil {
			return sandbox.Capabilities{}, operationErr
		}
		if qualified.Deployment != expected || qualified.Capabilities.RuntimeDigest != expected.RuntimeDigest {
			return sandbox.Capabilities{}, broker.ErrConflict
		}
		if checkErr := check(bounded); checkErr != nil {
			return sandbox.Capabilities{}, checkErr
		}
		if validationErr := qualified.Capabilities.Validate(); validationErr != nil {
			return sandbox.Capabilities{}, validationErr
		}
		return qualified.Capabilities, nil
	}, nil
}

func deploymentIdentity(configuration WorkerConfiguration) DeploymentIdentity {
	return DeploymentIdentity{Broker: configuration.Settings.Broker, ProfileDigest: configuration.ProfileDigest, RuntimeDigest: configuration.RuntimeDigest}
}
