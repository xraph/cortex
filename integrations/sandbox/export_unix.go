//go:build linux || darwin

package sandboxbroker

import "github.com/xraph/cortex/integrations/sandbox/internal/hostfs"

// ReadExport reads bounded regular content through no-follow descriptors after
// the workload has stopped. It rejects links, special files and traversal.
// The host must still verify content and authorize the export destination.
func ReadExport(directory, name string, maximum int64) ([]byte, error) {
	return hostfs.ReadRegular(directory, name, maximum)
}
