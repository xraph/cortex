package sandbox

import (
	"fmt"
	"mime"
	"path"
	"strings"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

// ArtifactProducer identifies worker-captured bytes, not a grading verdict.
const ArtifactProducer = "cortex/sandbox"

// Output declares a required regular file under the guest's scratch directory.
// The host allocates ID before sealing the job so an uncertain upload can be
// reconciled by identity. MediaType is a declaration, not content validation.
// Captured files are restricted evidence. No destination or release grant is
// carried here. Archives remain opaque bytes and are never expanded.
type Output struct {
	ID        id.ID  `json:"id"`
	Path      string `json:"path"`
	MediaType string `json:"media_type"`
	MaxBytes  int64  `json:"max_bytes"`
}

func (j Job) validateOutputs() error {
	if len(j.Outputs) > 16 {
		return fmt.Errorf("cortex: sandbox output count exceeds limit")
	}
	remaining := j.Limits.ArtifactBytes
	paths, identities := make(map[string]bool), make(map[string]bool)
	for _, input := range j.Inputs {
		identities[input.Ref.ID.String()] = true
	}
	for _, output := range j.Outputs {
		media, params, err := mime.ParseMediaType(output.MediaType)
		if err != nil || media != output.MediaType || len(params) != 0 || !text(media, 128) || !strings.Contains(media, "/") || strings.Contains(media, "*") || output.ID.IsNil() || output.ID.Prefix() != evidence.PrefixArtifact || identities[output.ID.String()] || paths[output.Path] || !strings.HasPrefix(output.Path, "/tmp/outputs/") || path.Clean(output.Path) != output.Path || !text(output.Path, 512) || output.MaxBytes <= 0 || output.MaxBytes > 1<<20 || output.MaxBytes > remaining {
			return fmt.Errorf("cortex: invalid sandbox output declaration")
		}
		remaining -= output.MaxBytes
		paths[output.Path], identities[output.ID.String()] = true, true
	}
	for name := range paths {
		for parent := path.Dir(name); parent != "/tmp/outputs"; parent = path.Dir(parent) {
			if paths[parent] {
				return fmt.Errorf("cortex: sandbox output file overlaps another output directory")
			}
		}
	}
	return nil
}

// OutputSource identifies the guest path in this job. It is an opaque provenance
// reference, never a host path or a URL to fetch. Callers must validate the job
// and require output to be one of its signed declarations.
func (j Job) OutputSource(output Output) string {
	return "sandbox:" + j.ID.String() + output.Path
}

// ValidateArtifacts binds captured references to the signed declarations. Set
// complete for an exited job; failed attempts may retain a verified subset.
// This checks metadata only. It grants no access and proves no task correctness.
func (j Job) ValidateArtifacts(refs []evidence.Ref, complete bool) error {
	if err := j.Validate(); err != nil {
		return err
	}
	if len(refs) > len(j.Outputs) || (complete && len(refs) != len(j.Outputs)) {
		return fmt.Errorf("cortex: sandbox captured output count mismatch")
	}
	declared := make(map[string]Output, len(j.Outputs))
	for _, output := range j.Outputs {
		declared[output.ID.String()] = output
	}
	for _, ref := range refs {
		if err := ref.Validate(); err != nil {
			return err
		}
		output, ok := declared[ref.ID.String()]
		if !ok || ref.Scope.Canonical() != j.Scope.Canonical() || ref.Bytes > output.MaxBytes || ref.MediaType != output.MediaType || ref.Version != j.Digest || ref.Producer != ArtifactProducer || ref.SourceURI != j.OutputSource(output) || ref.Sensitivity != evidence.SensitivityRestricted {
			return fmt.Errorf("cortex: sandbox artifact does not match output declaration")
		}
		delete(declared, ref.ID.String())
	}
	return nil
}
