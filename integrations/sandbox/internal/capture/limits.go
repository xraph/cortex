package capture

import (
	"fmt"
	"time"
)

// ValidateRetention requires an explicit host retention window of at most a day.
// A deployment may disable capture with zero, but cannot call Collect that way.
func ValidateRetention(retention time.Duration) error {
	if retention <= 0 || retention > 24*time.Hour {
		return fmt.Errorf("cortex: invalid sandbox artifact retention")
	}
	return nil
}
