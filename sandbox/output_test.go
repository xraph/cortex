package sandbox_test

import (
	"testing"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/sandbox"
)

func outputJob(t *testing.T) sandbox.Job {
	t.Helper()
	j := testJob()
	j.Limits.ArtifactBytes = 1024
	j.Outputs = []sandbox.Output{{ID: id.New(evidence.PrefixArtifact), Path: "/tmp/outputs/result.json", MediaType: "application/json", MaxBytes: 1024}}
	sealed, err := sandbox.Seal(j)
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

func TestOutputPlanIsBoundAndDetached(t *testing.T) {
	j := outputJob(t)
	frozen, err := sandbox.Seal(j)
	if err != nil {
		t.Fatal(err)
	}
	j.Outputs[0].Path = "/tmp/outputs/changed"
	if err = frozen.Validate(); err != nil {
		t.Fatal(err)
	}
	if err = j.Validate(); err == nil {
		t.Fatal("mutated output declaration accepted")
	}
}

func TestOutputPlanRejectsUnsafeRequests(t *testing.T) {
	cases := map[string]func(*sandbox.Job){
		"host path": func(j *sandbox.Job) { j.Outputs[0].Path = "/etc/passwd" },
		"traversal": func(j *sandbox.Job) { j.Outputs[0].Path = "/tmp/outputs/../secret" },
		"directory": func(j *sandbox.Job) { j.Outputs[0].Path = "/tmp/outputs/" },
		"duplicate path": func(j *sandbox.Job) {
			j.Limits.ArtifactBytes = 2048
			other := j.Outputs[0]
			other.ID = id.New(evidence.PrefixArtifact)
			j.Outputs = append(j.Outputs, other)
		},
		"duplicate identity": func(j *sandbox.Job) {
			j.Limits.ArtifactBytes = 2048
			other := j.Outputs[0]
			other.Path += "2"
			j.Outputs = append(j.Outputs, other)
		},
		"overlap": func(j *sandbox.Job) {
			j.Limits.ArtifactBytes = 2048
			other := j.Outputs[0]
			other.ID = id.New(evidence.PrefixArtifact)
			other.Path += "/nested"
			j.Outputs = append(j.Outputs, other)
		},
		"zero bytes":       func(j *sandbox.Job) { j.Outputs[0].MaxBytes = 0 },
		"per file bound":   func(j *sandbox.Job) { j.Outputs[0].MaxBytes = (1 << 20) + 1; j.Limits.ArtifactBytes = 2 << 20 },
		"aggregate":        func(j *sandbox.Job) { j.Limits.ArtifactBytes-- },
		"negative budget":  func(j *sandbox.Job) { j.Limits.ArtifactBytes = -1 },
		"excess budget":    func(j *sandbox.Job) { j.Limits.ArtifactBytes = (16 << 20) + 1 },
		"missing identity": func(j *sandbox.Job) { j.Outputs[0].ID = id.Nil },
		"invalid media":    func(j *sandbox.Job) { j.Outputs[0].MediaType = "not a media type" },
		"bare media":       func(j *sandbox.Job) { j.Outputs[0].MediaType = "text" },
		"wildcard media":   func(j *sandbox.Job) { j.Outputs[0].MediaType = "text/*" },
		"too many": func(j *sandbox.Job) {
			j.Limits.ArtifactBytes = 16 << 20
			for i := 0; i < 16; i++ {
				j.Outputs = append(j.Outputs, sandbox.Output{ID: id.New(evidence.PrefixArtifact), Path: j.Outputs[0].Path + id.New(evidence.PrefixArtifact).String(), MediaType: "text/plain", MaxBytes: 1})
			}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			j := outputJob(t)
			change(&j)
			if _, err := sandbox.Seal(j); err == nil {
				t.Fatal("unsafe output plan accepted")
			}
		})
	}
}

func TestOutputCaptureRequiresExplicitCapabilityBudget(t *testing.T) {
	j := outputJob(t)
	maximum := j.Limits
	maximum.ArtifactBytes = 0
	if j.Limits.Within(maximum) {
		t.Fatal("legacy grant permitted artifact capture")
	}
	maximum.ArtifactBytes = j.Limits.ArtifactBytes
	if !j.Limits.Within(maximum) {
		t.Fatal("explicit grant rejected")
	}
}

func TestCapturedOutputMustMatchDeclaration(t *testing.T) {
	j := outputJob(t)
	now := time.Now().UTC()
	a, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: j.Outputs[0].ID, Scope: j.Scope, MediaType: j.Outputs[0].MediaType, Version: j.Digest, Producer: sandbox.ArtifactProducer, SourceURI: j.OutputSource(j.Outputs[0]), Sensitivity: evidence.SensitivityRestricted, CreatedAt: now, RetainUntil: now.Add(time.Hour)}, Data: []byte("{}")})
	if err != nil {
		t.Fatal(err)
	}
	if err = j.ValidateArtifacts([]evidence.Ref{a.Ref}, true); err != nil {
		t.Fatal(err)
	}
	if err = j.ValidateArtifacts(nil, true); err == nil {
		t.Fatal("missing required output accepted")
	}
	if err = j.ValidateArtifacts(nil, false); err != nil {
		t.Fatal("failed attempt cannot omit artifacts")
	}
	for name, change := range map[string]func(*evidence.Ref){
		"identity":       func(r *evidence.Ref) { r.ID = id.New(evidence.PrefixArtifact) },
		"job":            func(r *evidence.Ref) { r.Version = "other" },
		"source":         func(r *evidence.Ref) { r.SourceURI = "other" },
		"producer":       func(r *evidence.Ref) { r.Producer = "guest" },
		"media":          func(r *evidence.Ref) { r.MediaType = "text/html" },
		"size":           func(r *evidence.Ref) { r.Bytes = 1025 },
		"classification": func(r *evidence.Ref) { r.Sensitivity = evidence.SensitivityPublic },
	} {
		t.Run(name, func(t *testing.T) {
			r := a.Ref
			change(&r)
			if validationErr := j.ValidateArtifacts([]evidence.Ref{r}, true); validationErr == nil {
				t.Fatal("unbound artifact accepted")
			}
		})
	}
	if err = j.ValidateArtifacts([]evidence.Ref{a.Ref, a.Ref}, false); err == nil {
		t.Fatal("duplicate artifact accepted")
	}
}

func TestOutputCannotReuseAnInputIdentity(t *testing.T) {
	j := outputJob(t)
	now := time.Now().UTC()
	a, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: j.Outputs[0].ID, Scope: j.Scope, MediaType: "text/plain", Version: "1", Producer: "host", Sensitivity: evidence.SensitivityRestricted, CreatedAt: now, RetainUntil: now.Add(time.Hour)}, Data: []byte("input")})
	if err != nil {
		t.Fatal(err)
	}
	j.Inputs = []sandbox.Input{{Path: "/inputs/data", Ref: a.Ref}}
	if _, err = sandbox.Seal(j); err == nil {
		t.Fatal("output may overwrite an input identity")
	}
}
