// Package sandbox defines bounded evaluation jobs without importing a runtime.
package sandbox

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

// PrefixJob identifies a sandbox evaluation job.
const PrefixJob id.Prefix = "sjob"

// NetworkDenied is the only network profile supported by schema one.
const NetworkDenied = "denied"

// Limits are hard ceilings. OutputBytes bounds combined stdout and stderr;
// ArtifactBytes separately bounds captured files and defaults to no capture.
type Limits struct {
	CPUMillis     int64         `json:"cpu_millis"`
	MemoryBytes   int64         `json:"memory_bytes"`
	Processes     int64         `json:"processes"`
	ScratchBytes  int64         `json:"scratch_bytes"`
	WallTime      time.Duration `json:"wall_time_ns"`
	OutputBytes   int64         `json:"output_bytes"`
	ArtifactBytes int64         `json:"artifact_bytes,omitempty"`
}

// Input mounts one immutable artifact under /inputs, read-only.
// Path never identifies a host path. Directories and links are not inputs.
type Input struct {
	Path string       `json:"path"`
	Ref  evidence.Ref `json:"ref"`
}

// Job carries no host mount, capability, credential or shell-interpolation fields.
// The host authenticates Principal and Scope independently of this payload.
type Job struct {
	SchemaVersion int               `json:"schema_version"`
	ID            id.ID             `json:"id"`
	Key           string            `json:"key"`
	Scope         cortex.Scope      `json:"scope"`
	Principal     string            `json:"principal"`
	Image         string            `json:"image"`
	Argv          []string          `json:"argv"`
	Inputs        []Input           `json:"inputs,omitempty"`
	Outputs       []Output          `json:"outputs,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	Network       string            `json:"network"`
	Limits        Limits            `json:"limits"`
	Digest        string            `json:"digest"`
}

// Seal detaches and hashes a schema-one job. It grants no execution authority.
func Seal(j Job) (Job, error) {
	if err := j.validateFields(); err != nil {
		return Job{}, err
	}
	j.Digest = ""
	data, err := json.Marshal(j)
	if err != nil {
		return Job{}, err
	}
	if len(data) > 1<<20 {
		return Job{}, fmt.Errorf("cortex: sandbox job exceeds one MiB")
	}
	var frozen Job
	if err := json.Unmarshal(data, &frozen); err != nil {
		return Job{}, err
	}
	frozen.Digest = evidence.Digest(data)
	return frozen, nil
}

// Validate detects mutation and rejects unsupported execution requests.
func (j Job) Validate() error {
	frozen, err := Seal(j)
	if err != nil {
		return err
	}
	if j.Digest != frozen.Digest {
		return fmt.Errorf("cortex: sandbox job digest mismatch")
	}
	return nil
}

func (j Job) validateFields() error {
	if !utf8.ValidString(j.Scope.Canonical()) || j.SchemaVersion != 1 || j.ID.IsNil() || j.ID.Prefix() != PrefixJob || !text(j.Key, 256) || !text(j.Principal, 512) || !strings.HasPrefix(j.Image, "sha256:") || !evidence.ValidDigest(strings.TrimPrefix(j.Image, "sha256:")) || j.Network != NetworkDenied || !j.Limits.valid() {
		return fmt.Errorf("cortex: invalid sandbox job")
	}
	if err := cortex.ValidateScope(j.Scope); err != nil {
		return err
	}
	if len(j.Argv) == 0 || len(j.Argv) > 256 || len(j.Inputs) > 256 || len(j.Env) > 64 {
		return fmt.Errorf("cortex: sandbox collection bound exceeded")
	}
	if err := j.validateOutputs(); err != nil {
		return err
	}
	for _, arg := range j.Argv {
		if !text(arg, 16384) {
			return fmt.Errorf("cortex: invalid sandbox argument")
		}
	}
	seen := make(map[string]bool, len(j.Inputs))
	for _, input := range j.Inputs {
		if !strings.HasPrefix(input.Path, "/inputs/") || path.Clean(input.Path) != input.Path || !text(input.Path, 512) || seen[input.Path] || input.Ref.Scope.Canonical() != j.Scope.Canonical() {
			return fmt.Errorf("cortex: invalid sandbox input")
		}
		if err := input.Ref.Validate(); err != nil {
			return err
		}
		seen[input.Path] = true
	}
	for name := range seen {
		for parent := path.Dir(name); parent != "/inputs"; parent = path.Dir(parent) {
			if seen[parent] {
				return fmt.Errorf("cortex: sandbox input file overlaps another input directory")
			}
		}
	}
	for key, value := range j.Env {
		// A small explicit set avoids loader, credential and proxy environment channels.
		if key != "LANG" && key != "LC_ALL" && key != "TZ" {
			return fmt.Errorf("cortex: unsupported sandbox environment key")
		}
		if !text(value, 256) {
			return fmt.Errorf("cortex: invalid sandbox environment value")
		}
	}
	return nil
}

func text(value string, maximum int) bool {
	return strings.TrimSpace(value) != "" && len(value) <= maximum && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}
func (l Limits) valid() bool {
	return l.CPUMillis > 0 && l.MemoryBytes > 0 && l.Processes > 0 && l.ScratchBytes > 0 && l.WallTime > 0 && l.OutputBytes > 0 && l.OutputBytes <= 1<<20 && l.CPUMillis <= 1_000_000 && l.Processes <= 1_000_000 && l.ArtifactBytes >= 0 && l.ArtifactBytes <= 16<<20
}

// Within reports whether every finite limit is within a host grant.
func (l Limits) Within(maximum Limits) bool {
	return l.valid() && maximum.valid() && l.CPUMillis <= maximum.CPUMillis && l.MemoryBytes <= maximum.MemoryBytes && l.Processes <= maximum.Processes && l.ScratchBytes <= maximum.ScratchBytes && l.WallTime <= maximum.WallTime && l.OutputBytes <= maximum.OutputBytes && l.ArtifactBytes <= maximum.ArtifactBytes
}
