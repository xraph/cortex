package audithook

import log "github.com/xraph/go-utils/log"

// Option configures the audit hook Extension.
type Option func(*Extension)

// WithActions limits which actions are recorded.
func WithActions(actions ...string) Option {
	return func(e *Extension) {
		e.enabled = make(map[string]bool, len(actions))
		for _, a := range actions {
			e.enabled[a] = true
		}
	}
}

// WithLogger sets a custom logger for the extension.
func WithLogger(l log.Logger) Option {
	return func(e *Extension) {
		e.logger = l
	}
}
