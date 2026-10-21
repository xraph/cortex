package sandboxbroker_test

import (
	"testing"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/sandbox"
)

func TestRuntimeCannotInventOrOmitDeclaredArtifacts(t *testing.T) {
	j := job(t)
	j.Limits.ArtifactBytes = 1024
	j.Outputs = []sandbox.Output{{ID: id.New(evidence.PrefixArtifact), Path: "/tmp/outputs/report", MediaType: "text/plain", MaxBytes: 1024}}
	j, err := sandbox.Seal(j)
	if err != nil {
		t.Fatal(err)
	}
	r := sandbox.Result{JobDigest: j.Digest, State: sandbox.StateExited}
	if err = broker.ValidateRuntimeResult(r, j); err == nil {
		t.Fatal("exited result omitted required artifacts")
	}
	r.State = sandbox.StateFailed
	if err = broker.ValidateRuntimeResult(r, j); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	a, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: j.Outputs[0].ID, Scope: j.Scope, MediaType: "text/plain", Version: j.Digest, Producer: sandbox.ArtifactProducer, SourceURI: j.OutputSource(j.Outputs[0]), Sensitivity: evidence.SensitivityRestricted, CreatedAt: now, RetainUntil: now.Add(time.Hour)}, Data: []byte("untrusted output")})
	if err != nil {
		t.Fatal(err)
	}
	r.State, r.Artifacts = sandbox.StateExited, []evidence.Ref{a.Ref}
	if err = broker.ValidateRuntimeResult(r, j); err != nil {
		t.Fatal(err)
	}
	r.State = sandbox.StateRunning
	if err = broker.ValidateRuntimeResult(r, j); err == nil {
		t.Fatal("running job exposed mutable output")
	}
	r.State = sandbox.StateExited
	r.Artifacts[0].ID = id.New(evidence.PrefixArtifact)
	if err = broker.ValidateRuntimeResult(r, j); err == nil {
		t.Fatal("undeclared artifact accepted")
	}
}
