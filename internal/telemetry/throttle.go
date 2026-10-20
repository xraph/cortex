package telemetry

import (
	"sync"
	"time"
)

// LogInterval is how often one log site may write a line. Failures in
// between are counted and reported on the next line, so a store that is
// down fails every step and every run, and the log gets one line a minute
// per site rather than one per step.
const LogInterval = time.Minute

// Throttle limits log lines per key. Keys must come from a bounded set (a
// call site's message, or a hook and the name of a registered extension),
// which is what bounds the map. The zero value is ready to use and safe for
// concurrent use.
type Throttle struct {
	mu    sync.Mutex
	state map[string]*throttleState
}

type throttleState struct {
	last       time.Time
	suppressed int
}

// Allow reports whether key may log at now, and how many lines it skipped
// since the last one it wrote.
func (t *Throttle) Allow(key string, now time.Time) (ok bool, suppressed int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state == nil {
		t.state = make(map[string]*throttleState)
	}
	st, found := t.state[key]
	if !found {
		st = &throttleState{}
		t.state[key] = st
	}
	if !st.last.IsZero() && now.Sub(st.last) < LogInterval {
		st.suppressed++
		return false, 0
	}
	suppressed = st.suppressed
	st.last, st.suppressed = now, 0
	return true, suppressed
}
