//go:build linux && sandbox_peer_kernel

package service

import (
	"context"
	"os"
	"testing"
)

func TestInstalledAttestationKernelConstructor(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("installed attestation fixture requires root")
	}
	name, _, _ := workerConfigFixture(t)
	attest, err := NewDeploymentAttestation(t.Context(), name, func(_ context.Context, identity DeploymentIdentity) (DeploymentQualification, error) {
		return qualificationFixture(identity), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = attest(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(name, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = attest(t.Context()); err == nil {
		t.Fatal("production constructor retained writable configuration")
	}
}
