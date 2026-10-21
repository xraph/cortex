package image

import (
	"fmt"
	"time"
)

// Limits bounds aggregate image staging work across all layers. The outer
// workspace filesystem enforces an independent storage and inode ceiling.
type Limits struct {
	CompressedBytes int64         `json:"compressed_bytes"`
	ExpandedBytes   int64         `json:"expanded_bytes"`
	Entries         int           `json:"entries"`
	Timeout         time.Duration `json:"timeout_ns"`
}

// Validate refuses absent or unsupported image staging budgets.
func (l Limits) Validate() error {
	if l.CompressedBytes <= 0 || l.CompressedBytes > 4<<30 || l.ExpandedBytes <= 0 || l.ExpandedBytes > 8<<30 || l.Entries <= 0 || l.Entries > 100_000 || l.Timeout <= 0 || l.Timeout > 10*time.Minute {
		return fmt.Errorf("cortex: invalid image staging limits")
	}
	return nil
}
