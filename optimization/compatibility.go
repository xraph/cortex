package optimization

import "context"

// Compatibility checks current source, tool and host policy revisions. Hosts
// must return incompatible when any required revision cannot be established.
type Compatibility interface {
	Check(context.Context, HarnessVersion) (CompatibilityReport, error)
}

// CompatibilityReport binds a point-in-time observation to an exact version.
type CompatibilityReport struct {
	Version      VersionRef `json:"version"`
	PolicyDigest string     `json:"policy_digest"`
	Compatible   bool       `json:"compatible"`
}
