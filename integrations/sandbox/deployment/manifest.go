// Package deployment verifies the complete host-selected runsc installation.
// Matching bytes establish deployment identity, not isolation qualification.
package deployment

import (
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/xraph/cortex/evidence"
)

const (
	maxFiles      = 128
	maxFileBytes  = int64(2 << 30)
	maxTotalBytes = int64(4 << 30)
)

// File pins one executable's relative path, content, size and Unix permissions.
// Sidecars must remain beside runsc under gvisor-bin. Links are not supported.
type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	Mode   uint32 `json:"mode"`
}

// Manifest is trusted host configuration obtained from an approved release.
// Never derive the expected hashes from the installation being checked. The
// optional containerd shim and every installed sidecar must be inventoried.
type Manifest struct {
	SchemaVersion int    `json:"schema_version"`
	Architecture  string `json:"architecture"`
	Files         []File `json:"files"`
}

// Digest validates and hashes a detached, path-sorted inventory. A legacy
// single-binary release is valid only when no sidecar directory is installed.
func (m Manifest) Digest() (string, error) {
	if m.SchemaVersion != 1 || (m.Architecture != "amd64" && m.Architecture != "arm64") || len(m.Files) == 0 || len(m.Files) > maxFiles {
		return "", fmt.Errorf("cortex: invalid runtime deployment manifest")
	}
	m.Files = slices.Clone(m.Files)
	slices.SortFunc(m.Files, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
	seen := make(map[string]bool, len(m.Files))
	var total int64
	for _, file := range m.Files {
		if !validPath(file.Path) || seen[file.Path] || !evidence.ValidDigest(file.SHA256) || file.Bytes <= 0 || file.Bytes > maxFileBytes || (file.Mode != 0o755 && file.Mode != 0o555) {
			return "", fmt.Errorf("cortex: invalid runtime deployment file")
		}
		for parent := path.Dir(file.Path); parent != "."; parent = path.Dir(parent) {
			if seen[parent] {
				return "", fmt.Errorf("cortex: runtime file conflicts with a directory")
			}
		}
		seen[file.Path] = true
		total += file.Bytes
	}
	if !seen["runsc"] || total > maxTotalBytes {
		return "", fmt.Errorf("cortex: incomplete or oversized runtime deployment")
	}
	body, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return evidence.Digest(body), nil
}

func validPath(name string) bool {
	if len(name) > 512 || path.Clean(name) != name || strings.Count(name, "/") > 8 {
		return false
	}
	if name != "runsc" && name != "containerd-shim-runsc-v1" && !strings.HasPrefix(name, "gvisor-bin/") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
		for _, char := range part {
			if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '.' || char == '_' || char == '-' {
				continue
			}
			return false
		}
	}
	return true
}
