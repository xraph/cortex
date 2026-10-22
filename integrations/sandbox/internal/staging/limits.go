package staging

import "fmt"

// Limits bounds each input and aggregate input staging bytes. The outer
// workspace supplies the independent filesystem capacity ceiling.
type Limits struct {
	PerInputBytes int64 `json:"per_input_bytes"`
	TotalBytes    int64 `json:"total_bytes"`
}

// Validate refuses absent or unsupported input staging budgets.
func (l Limits) Validate() error {
	if l.PerInputBytes <= 0 || l.PerInputBytes > 1<<30 || l.TotalBytes <= 0 || l.TotalBytes > 4<<30 {
		return fmt.Errorf("cortex: invalid input staging limits")
	}
	return nil
}
