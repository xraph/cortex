//go:build linux

package worker

import (
	"context"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/sandbox/internal/capture"
	"github.com/xraph/cortex/integrations/sandbox/internal/image"
	"github.com/xraph/cortex/integrations/sandbox/internal/staging"
	"github.com/xraph/cortex/sandbox"
)

type evidenceFixtureSession struct {
	*fixtureSession
	images image.Source
	input  staging.Source
	output capture.Repository
}

func (s *evidenceFixtureSession) ImageSource() image.Source { return s.images }

func (s *evidenceFixtureSession) InputSource() staging.Source            { return s.input }
func (s *evidenceFixtureSession) ArtifactRepository() capture.Repository { return s.output }

func TestWorkerSourcesUseSessionCapabilitiesAndSignedBindings(t *testing.T) {
	s, _, _, _ := pipelineFixture(t, "")
	now := time.Now().UTC()
	input, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: id.New(evidence.PrefixArtifact), Scope: s.job.Scope, MediaType: "text/plain", Version: "1", Producer: "fixture", Sensitivity: evidence.SensitivityRestricted, CreatedAt: now, RetainUntil: now.Add(time.Hour)}, Data: []byte("input")})
	if err != nil {
		t.Fatal(err)
	}
	s.job.Inputs = []sandbox.Input{{Path: "/inputs/data", Ref: input.Ref}}
	s.job.Outputs = []sandbox.Output{{ID: id.New(evidence.PrefixArtifact), Path: "/tmp/outputs/report", MediaType: "text/plain", MaxBytes: 64}}
	s.job.Limits.ArtifactBytes = 64
	s.job, err = sandbox.Seal(s.job)
	if err != nil {
		t.Fatal(err)
	}
	output, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: s.job.Outputs[0].ID, Scope: s.job.Scope, MediaType: "text/plain", Version: s.job.Digest, Producer: sandbox.ArtifactProducer, SourceURI: s.job.OutputSource(s.job.Outputs[0]), Sensitivity: evidence.SensitivityRestricted, CreatedAt: now, RetainUntil: now.Add(time.Hour)}, Data: []byte("result")})
	if err != nil {
		t.Fatal(err)
	}
	session := &evidenceFixtureSession{fixtureSession: s, images: imageSource{}, input: inputSource{}, output: &captureRepository{}}
	bound := bindWorkerSources(session)
	if bound.Images != session.images || bound.Inputs != session.input || bound.Artifacts != session.output {
		t.Fatal("worker sources bypassed session")
	}
	ctx := cortex.WithScope(t.Context(), s.job.Scope)
	if err = bound.AuthorizeImage(ctx, s.job, s.job.Image); err != nil {
		t.Fatal(err)
	}
	if err = bound.AuthorizeImage(context.Background(), s.job, s.job.Image); err == nil {
		t.Fatal("unscoped image accepted")
	}
	if err = bound.AuthorizeImage(ctx, s.job, "sha256:foreign"); err == nil {
		t.Fatal("foreign image accepted")
	}
	changedJob := s.job
	changedJob.Argv = []string{"different"}
	changedJob, err = sandbox.Seal(changedJob)
	if err != nil {
		t.Fatal(err)
	}
	if err = bound.AuthorizeImage(ctx, changedJob, changedJob.Image); err == nil {
		t.Fatal("foreign job image accepted")
	}

	if err = bound.AuthorizeInput(ctx, s.job, evidence.ActionRead, input.Ref); err != nil {
		t.Fatal(err)
	}
	for _, action := range []evidence.Action{evidence.ActionPut, evidence.ActionRead} {
		if err = bound.AuthorizeOutput(ctx, s.job, action, output.Ref); err != nil {
			t.Fatal(err)
		}
	}
	if err = bound.AuthorizeInput(ctx, s.job, evidence.ActionPut, input.Ref); err == nil {
		t.Fatal("input write policy accepted")
	}
	changed := input.Ref
	changed.Version = "changed"
	if err = bound.AuthorizeInput(ctx, s.job, evidence.ActionRead, changed); err == nil {
		t.Fatal("substituted input accepted")
	}
	changed = output.Ref
	changed.SourceURI = "foreign"
	if err = bound.AuthorizeOutput(ctx, s.job, evidence.ActionPut, changed); err == nil {
		t.Fatal("substituted output accepted")
	}
	if err = bound.AuthorizeInput(context.Background(), s.job, evidence.ActionRead, input.Ref); err == nil {
		t.Fatal("missing scope accepted")
	}
	s.fail = "authorize"
	if err = bound.AuthorizeImage(ctx, s.job, s.job.Image); err == nil {
		t.Fatal("revoked image accepted")
	}
	if err = bound.AuthorizeInput(ctx, s.job, evidence.ActionRead, input.Ref); err == nil {
		t.Fatal("revoked worker accepted")
	}
}
