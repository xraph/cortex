//go:build linux || darwin

package service

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"time"
)

// Probe output is trusted only with its producer signature and retained pin.
// Parse test2json events, never human-facing output text or a boolean in policy.
func passedQualificationProbe(body []byte, probe QualificationProbe, observedAt time.Time, maxAge time.Duration) (time.Time, bool) {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 4096), 64<<10)
	earliest := observedAt
	started, ran, passed, finished := false, false, false, false
	const (
		running = iota + 1
		paused
		complete
	)
	states := make(map[string]int)
	for scanner.Scan() {
		if finished {
			return time.Time{}, false
		}
		var event struct {
			Time        time.Time
			Action      string
			Package     string
			Test        string  `json:",omitempty"`
			Elapsed     float64 `json:",omitempty"`
			Output      string  `json:",omitempty"`
			FailedBuild string  `json:",omitempty"`
		}
		decoder := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&event) != nil || event.Package != probe.Package || event.FailedBuild != "" || event.Time.IsZero() || event.Time.After(observedAt) || observedAt.Sub(event.Time) > maxAge {
			return time.Time{}, false
		}
		if event.Time.Before(earliest) {
			earliest = event.Time
		}
		// Reject duplicate keys and trailing objects without imposing Go's field order.
		var fields map[string]json.RawMessage
		if json.Unmarshal(scanner.Bytes(), &fields) != nil || !uniqueProbeFields(scanner.Bytes(), len(fields)) {
			return time.Time{}, false
		}
		switch event.Action {
		case "start":
			if started || event.Test != "" {
				return time.Time{}, false
			}
			started = true
		case "run":
			if !started || passed || (event.Test != probe.Test && !strings.HasPrefix(event.Test, probe.Test+"/")) {
				return time.Time{}, false
			}
			if _, exists := states[event.Test]; exists {
				return time.Time{}, false
			}
			if event.Test != probe.Test && !ran {
				return time.Time{}, false
			}
			for name, state := range states {
				if strings.HasPrefix(event.Test, name+"/") && state == complete {
					return time.Time{}, false
				}
			}
			states[event.Test] = running
			if event.Test == probe.Test {
				ran = true
			}
		case "pass":
			if event.Test == "" {
				if !passed {
					return time.Time{}, false
				}
				for _, done := range states {
					if done != complete {
						return time.Time{}, false
					}
				}
				finished = true
			} else {
				state := states[event.Test]
				if state != running {
					return time.Time{}, false
				}
				for name, childState := range states {
					if strings.HasPrefix(name, event.Test+"/") && childState != complete {
						return time.Time{}, false
					}
				}
				states[event.Test] = complete
				if event.Test == probe.Test {
					passed = true
				}
			}
		case "output":
			if !started || (event.Test != "" && states[event.Test] != running && states[event.Test] != paused) {
				return time.Time{}, false
			}
		case "pause":
			if states[event.Test] != running {
				return time.Time{}, false
			}
			states[event.Test] = paused
		case "cont":
			if states[event.Test] != paused {
				return time.Time{}, false
			}
			states[event.Test] = running
		default:
			return time.Time{}, false // Fail, skip, build failures and unknown actions deny.
		}
	}
	return earliest, scanner.Err() == nil && started && ran && passed && finished
}
func uniqueProbeFields(body []byte, count int) bool {
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return false
	}
	seen := make(map[string]bool, count)
	for decoder.More() {
		key, keyErr := decoder.Token()
		name, ok := key.(string)
		if keyErr != nil || !ok || seen[name] {
			return false
		}
		switch name {
		case "Time", "Action", "Package", "Test", "Elapsed", "Output", "FailedBuild":
		default:
			return false
		}
		seen[name] = true
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return false
		}
	}
	_, err = decoder.Token()
	return err == nil && len(seen) == count
}
