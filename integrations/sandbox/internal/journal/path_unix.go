//go:build linux || darwin

package journal

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
)

func preparePath(filename string) (string, error) {
	if !filepath.IsAbs(filename) || filepath.Clean(filename) != filename || strings.Contains(filename, "?") {
		return "", fmt.Errorf("cortex: sandbox journal requires an absolute file path")
	}
	// Resolve the configured directory once (including macOS temporary-directory
	// aliases), then use only this checked canonical path for SQLite access.
	parent, err := filepath.EvalSymlinks(filepath.Dir(filename))
	if err != nil {
		return "", err
	}
	owner := os.Geteuid()
	if owner < 0 || int64(owner) > math.MaxUint32 {
		return "", fmt.Errorf("cortex: invalid journal owner")
	}
	canonical := filepath.Join(parent, filepath.Base(filename))
	file, err := hostfs.OpenPrivateState(canonical, uint32(owner), true)
	if err != nil {
		return "", err
	}
	return canonical, file.Close()
}
