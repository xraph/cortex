package a2aremote

import (
	"context"
	"time"
)

// DefaultStreamPoll is how often a subscription re-reads a task.
const DefaultStreamPoll = 500 * time.Millisecond

// StreamEvent is one thing a subscriber is told.
//
// Exactly one field is set, mirroring the protocol's own oneof. A task
// is the opening snapshot, a status update is a transition, and an
// artifact update carries output as it appears.
type StreamEvent struct {
	Task           *Task                  `json:"task,omitempty"`
	StatusUpdate   *TaskStatusUpdateEvent `json:"statusUpdate,omitempty"`
	ArtifactUpdate *TaskArtifactUpdate    `json:"artifactUpdate,omitempty"`
	Message        *Message               `json:"message,omitempty"`
}

// TaskStatusUpdateEvent says a task moved.
type TaskStatusUpdateEvent struct {
	TaskID    string     `json:"taskId"`
	ContextID string     `json:"contextId,omitempty"`
	Status    TaskStatus `json:"status"`
	// Final says no more events follow, which is what tells a client to
	// stop reading rather than to keep a dead connection open.
	Final bool `json:"final"`
}

// TaskArtifactUpdate carries a task output as it appears.
type TaskArtifactUpdate struct {
	TaskID    string   `json:"taskId"`
	ContextID string   `json:"contextId,omitempty"`
	Artifact  Artifact `json:"artifact"`
}

// Emit is how a binding receives events. Returning an error stops the
// stream, which is how a disconnected client ends its own subscription.
type Emit func(StreamEvent) error

// StreamMessage sends a message and then follows the work it started.
//
// The first event is the task as it exists the moment the message is
// accepted, so a client has something to hold on to immediately. After
// that come status transitions, and the last event carries the artifacts
// with Final set.
//
// What this is not is token-level streaming. A2A's streaming is about a
// task's progress, and that is what this reports: cortex learns of a
// transition by re-reading the task on an interval, and the client sees
// the transitions in order either way. The interval bounds the latency
// and nothing else.
func (s *Service) StreamMessage(ctx context.Context, cred Credentials, req SendMessageRequest, emit Emit) error {
	result, err := s.SendMessage(ctx, cred, req)
	if err != nil {
		return err
	}

	// An informative starts no work, so the acknowledgement is the whole
	// stream. Holding the connection open for a message nobody is acting
	// on would be a subscription to nothing.
	if result.Task == nil {
		return emit(StreamEvent{Message: result.Message})
	}
	if err := emit(StreamEvent{Task: result.Task}); err != nil {
		return err
	}
	return s.follow(ctx, cred, req.Tenant, result.Task.ID, result.Task.Status.State, emit)
}

// SubscribeTask follows a task that is already running.
func (s *Service) SubscribeTask(ctx context.Context, cred Credentials, tenant, taskID string, emit Emit) error {
	task, err := s.GetTask(ctx, cred, GetTaskRequest{Tenant: tenant, ID: taskID})
	if err != nil {
		return err
	}
	// A finished task has nothing to subscribe to. The protocol says so
	// too: subscribing to a terminal task is an unsupported operation
	// rather than an empty stream.
	if task.Status.State.Terminal() {
		return ErrUnsupportedOperation("SubscribeToTask on a task that has already finished")
	}
	if err := emit(StreamEvent{Task: task}); err != nil {
		return err
	}
	return s.follow(ctx, cred, tenant, task.ID, task.Status.State, emit)
}

// follow watches one task until it stops moving.
func (s *Service) follow(ctx context.Context, cred Credentials, tenant, taskID string, from TaskState, emit Emit) error {
	interval := s.opts.StreamPoll
	if interval <= 0 {
		interval = DefaultStreamPoll
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	last := from
	for {
		select {
		case <-ctx.Done():
			// The client went away. That ends its subscription and
			// nothing else: the task carries on without it.
			return ctx.Err()
		case <-ticker.C:
		}

		task, err := s.GetTask(ctx, cred, GetTaskRequest{Tenant: tenant, ID: taskID})
		if err != nil {
			return err
		}
		if task.Status.State == last {
			continue
		}
		last = task.Status.State

		final := task.Status.State.Terminal()
		if err := emit(StreamEvent{StatusUpdate: &TaskStatusUpdateEvent{
			TaskID:    task.ID,
			ContextID: task.ContextID,
			Status:    task.Status,
			Final:     final,
		}}); err != nil {
			return err
		}
		if !final {
			continue
		}

		// The output arrives with the last transition rather than after
		// it, so a client that stops reading on Final has everything.
		for i := range task.Artifacts {
			if err := emit(StreamEvent{ArtifactUpdate: &TaskArtifactUpdate{
				TaskID:    task.ID,
				ContextID: task.ContextID,
				Artifact:  task.Artifacts[i],
			}}); err != nil {
				return err
			}
		}
		return nil
	}
}
