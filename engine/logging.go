package engine

import (
	"time"

	log "github.com/xraph/go-utils/log"

	"github.com/xraph/cortex/internal/telemetry"
)

// The engine's own log lines go through these two helpers, and the rules
// they enforce are the reason they exist. In a Forge app the engine logs
// through the app's logger, so every line is live:
//
//   - A failure is logged by the type of its error (error_type), never by
//     its text. Store errors quote the values they choked on, hook errors
//     quote whatever the hook was handed, and a provider error can quote the
//     prompt.
//   - Fields are IDs, counts and enums. No input, output, prompt, tool
//     argument or result, scope value, principal, or agent, persona or
//     skill name.
//   - Each message is throttled to one line per telemetry.LogInterval, with
//     the number of lines it skipped. Most of these fire per step or per run,
//     and an outage would otherwise write one line for every one of them.
//
// The message is the throttle key, so it must be a constant.

func (e *Engine) logError(msg string, err error, fields ...log.Field) {
	if line, ok := e.throttled(msg, err, fields); ok {
		e.logger.Error(msg, line...)
	}
}

func (e *Engine) logWarn(msg string, err error, fields ...log.Field) {
	if line, ok := e.throttled(msg, err, fields); ok {
		e.logger.Warn(msg, line...)
	}
}

func (e *Engine) throttled(msg string, err error, fields []log.Field) ([]log.Field, bool) {
	ok, suppressed := e.logThrottle.Allow(msg, time.Now())
	if !ok {
		return nil, false
	}
	if err != nil {
		fields = append(fields, log.String("error_type", telemetry.ErrorType(err)))
	}
	if suppressed > 0 {
		fields = append(fields, log.Int("suppressed", suppressed))
	}
	return fields, true
}
