package contract

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	dc "github.com/xraph/forge/extensions/dashboard/contract"
	"github.com/xraph/forge/extensions/dashboard/contract/dispatcher"
)

// Stream events are a bounded, short-lived view of a live engine execution.
// Run, step and conversation records remain in the domain store after expiry.
// Polling this cursor through the contract retains its principal/CSRF boundary.
type liveRun struct {
	mu      sync.Mutex
	events  []engine.StreamEvent
	base    int
	done    bool
	expires time.Time
	cancel  context.CancelFunc
}
type StreamInput struct {
	ID    string `json:"id"`
	After int    `json:"after"`
}
type StreamOutput struct {
	Events    []engine.StreamEvent `json:"events"`
	Next      int                  `json:"next"`
	Lost      bool                 `json:"lost"`
	Done      bool                 `json:"done"`
	Available bool                 `json:"available"`
}

func (r *liveRun) append(ev engine.StreamEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	if len(r.events) > 1024 {
		r.events = r.events[1:]
		r.base++
	}
}
func (s *service) bindStream(d *dispatcher.Dispatcher) error {
	if err := command(d, s, "runs.start", "run", func(ctx context.Context, in ExecuteInput) (map[string]string, error) {
		if strings.TrimSpace(in.Input) == "" || len(in.Input) > 200000 {
			return nil, bad("Input must contain 1 to 200000 bytes")
		}
		if s.deps.Engine.LLM() == nil {
			return nil, &dc.Error{Code: dc.CodeUnavailable, Message: "No LLM client is installed. Echo fallback is disabled on the dashboard."}
		}
		ctx, ag, o, err := s.prepareExecution(ctx, in)
		if err != nil {
			return nil, err
		}
		if !ag.Enabled {
			return nil, &dc.Error{Code: dc.CodeConflict, Message: "This agent is disabled"}
		}
		s.streamMu.Lock()
		if s.streams == nil {
			s.streams = map[string]*liveRun{}
		}
		for key, r := range s.streams {
			r.mu.Lock()
			expired := r.done && time.Now().After(r.expires)
			r.mu.Unlock()
			if expired {
				delete(s.streams, key)
			}
		}
		if len(s.streams) >= 64 {
			s.streamMu.Unlock()
			return nil, &dc.Error{Code: dc.CodeUnavailable, Message: "Live run capacity reached. Try again after completed streams expire.", Retryable: true}
		}
		// Serialize allocation until the engine has supplied the durable run ID.
		runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		events := make(chan engine.StreamEvent, 32)
		r := &liveRun{cancel: cancel}
		if err = s.deps.Engine.StreamAgent(runCtx, ag.Name, in.Input, o, events); err != nil {
			cancel()
			s.streamMu.Unlock()
			return nil, err
		}
		first, ok := <-events
		if !ok || first.Type != engine.EventRunStarted {
			cancel()
			s.streamMu.Unlock()
			go func() {
				for range events {
				}
			}()
			return nil, &dc.Error{Code: dc.CodeUnavailable, Message: "The engine did not start a run"}
		}
		raw, _ := first.Data["run_id"].(string)
		if _, err = id.ParseAgentRunID(raw); err != nil {
			cancel()
			s.streamMu.Unlock()
			go func() {
				for range events {
				}
			}()
			return nil, bad("The engine returned an invalid run identity")
		}
		r.append(first)
		s.streams[raw] = r
		s.streamMu.Unlock()
		go func() {
			defer cancel()
			for ev := range events {
				r.append(ev)
			}
			r.mu.Lock()
			r.done = true
			r.expires = time.Now().Add(10 * time.Minute)
			r.mu.Unlock()
		}()
		return map[string]string{"id": raw}, nil
	}); err != nil {
		return err
	}
	return query(d, s, "runs.events", func(ctx context.Context, in StreamInput) (StreamOutput, error) {
		out := StreamOutput{Events: []engine.StreamEvent{}, Done: true}
		v, err := id.ParseAgentRunID(in.ID)
		if err != nil {
			return out, bad("Invalid run ID")
		}
		if in.After < 0 {
			return out, bad("Invalid stream cursor")
		}
		if _, err = s.deps.Engine.GetRun(ctx, v); err != nil {
			return out, err
		}
		s.streamMu.Lock()
		r := s.streams[in.ID]
		s.streamMu.Unlock()
		if r == nil {
			return out, nil
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		start := in.After - r.base
		out.Lost = start < 0
		if start < 0 {
			start = 0
		}
		if start > len(r.events) {
			return out, bad("Stream cursor is ahead of the run")
		}
		out.Events = append(out.Events, r.events[start:]...)
		out.Next = r.base + len(r.events)
		out.Done = r.done
		out.Available = true
		return out, nil
	})
}
func (s *service) cancelStream(raw string) {
	s.streamMu.Lock()
	r := s.streams[raw]
	s.streamMu.Unlock()
	if r != nil {
		r.cancel()
	}
}
