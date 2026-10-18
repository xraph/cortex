package safety

import "errors"

// Mode controls when approved content may leave the engine.
type Mode string

const (
	ModeBuffered        Mode = "buffered"
	ModeCheckedSegments Mode = "checked_segments"
	ModeDisabled        Mode = "disabled_explicit"
)

// Policy is host configuration, never instructions read from model or tool text.
// Byte limits apply even when scanning is explicitly disabled. Zero selects the
// bounded default. Required refuses execution when no gate is configured.
type Policy struct {
	Version            int  `json:"version"`
	Mode               Mode `json:"mode"`
	Required           bool `json:"required"`
	MaxInputBytes      int  `json:"max_input_bytes"`
	MaxOutputBytes     int  `json:"max_output_bytes"`
	MaxToolOutputBytes int  `json:"max_tool_output_bytes"`
}

// Resolve validates the policy and fills in finite limits.
func (p Policy) Resolve() (Policy, error) {
	if p.Version == 0 {
		p.Version = 1
	}
	if p.Version != 1 {
		return Policy{}, ErrUnavailable
	}
	if p.Mode == "" {
		p.Mode = ModeBuffered
	}
	switch p.Mode {
	case ModeBuffered, ModeCheckedSegments, ModeDisabled:
	default:
		return Policy{}, ErrUnavailable
	}
	if p.Required && p.Mode == ModeDisabled {
		return Policy{}, ErrUnavailable
	}
	if p.MaxInputBytes < 0 || p.MaxOutputBytes < 0 || p.MaxToolOutputBytes < 0 {
		return Policy{}, ErrLimit
	}
	if p.MaxInputBytes == 0 {
		p.MaxInputBytes = 1 << 20
	}
	if p.MaxOutputBytes == 0 {
		p.MaxOutputBytes = 1 << 20
	}
	if p.MaxToolOutputBytes == 0 {
		p.MaxToolOutputBytes = 1 << 20
	}
	return p, nil
}

var (
	ErrBlocked     = errors.New("cortex: content blocked by publication policy")
	ErrUnavailable = errors.New("cortex: publication policy unavailable")
	ErrLimit       = errors.New("cortex: publication byte limit exceeded")
)
