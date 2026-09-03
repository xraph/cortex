package a2aremote

import "time"

// FIPAExtensionURI identifies the extension that carries FIPA-ACL
// semantics over A2A. It is declared in every card this package serves,
// as an optional extension, and named in the extensions list of every
// message that uses it.
const FIPAExtensionURI = "https://cortex.xraph.dev/a2a/extensions/fipa-acl/v1"

// Role identifies who sent a message. The values are the protocol's own
// enum names rather than friendlier spellings.
type Role string

// The message roles.
const (
	RoleUser  Role = "ROLE_USER"
	RoleAgent Role = "ROLE_AGENT"
)

// Part is one piece of a message's content. Exactly one of the three
// shapes is set.
//
// Only text is understood today. A file or data part is refused with
// ContentTypeNotSupportedError rather than dropped, because a peer whose
// attachment vanished silently has no way to find out why the answer
// made no sense.
type Part struct {
	Text string    `json:"text,omitempty"`
	File *FilePart `json:"file,omitempty"`
	Data *DataPart `json:"data,omitempty"`
}

// FilePart carries a file, either inline or by reference.
type FilePart struct {
	Raw      []byte `json:"raw,omitempty"`
	URL      string `json:"url,omitempty"`
	MIMEType string `json:"mimeType,omitempty"`
	Name     string `json:"name,omitempty"`
}

// DataPart carries structured JSON.
type DataPart struct {
	Data map[string]any `json:"data,omitempty"`
}

// Message is one A2A message.
//
// ContextID is where a cortex conversation id travels, and TaskID is
// where a run id does. Both are native protocol fields, so nothing about
// them is a cortex convention a peer has to learn.
type Message struct {
	MessageID        string         `json:"messageId"`
	ContextID        string         `json:"contextId,omitempty"`
	TaskID           string         `json:"taskId,omitempty"`
	Role             Role           `json:"role"`
	Parts            []Part         `json:"parts"`
	Metadata         map[string]any `json:"metadata,omitempty"`
	Extensions       []string       `json:"extensions,omitempty"`
	ReferenceTaskIDs []string       `json:"referenceTaskIds,omitempty"`
}

// TaskState is where a task is in its lifecycle.
type TaskState string

// The eight task states.
const (
	TaskStateSubmitted     TaskState = "TASK_STATE_SUBMITTED"
	TaskStateWorking       TaskState = "TASK_STATE_WORKING"
	TaskStateCompleted     TaskState = "TASK_STATE_COMPLETED"
	TaskStateFailed        TaskState = "TASK_STATE_FAILED"
	TaskStateCanceled      TaskState = "TASK_STATE_CANCELED"
	TaskStateRejected      TaskState = "TASK_STATE_REJECTED"
	TaskStateInputRequired TaskState = "TASK_STATE_INPUT_REQUIRED"
	TaskStateAuthRequired  TaskState = "TASK_STATE_AUTH_REQUIRED"
)

// Terminal reports whether the task has finished for good.
//
// The two interrupted states are deliberately not terminal. A client
// that stopped polling on input-required would abandon a task that is
// about to carry on the moment somebody answers it.
func (s TaskState) Terminal() bool {
	switch s {
	case TaskStateCompleted, TaskStateFailed, TaskStateCanceled, TaskStateRejected:
		return true
	default:
		return false
	}
}

// TaskStatus is a task's current state, with an optional message saying
// something about it: why it failed, or what it is waiting for.
type TaskStatus struct {
	State     TaskState `json:"state"`
	Message   *Message  `json:"message,omitempty"`
	Timestamp string    `json:"timestamp,omitempty"`
}

// Artifact is a task output.
type Artifact struct {
	ArtifactID  string         `json:"artifactId"`
	Name        string         `json:"name,omitempty"`
	Description string         `json:"description,omitempty"`
	Parts       []Part         `json:"parts"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// Task is a stateful unit of work, as a peer sees it.
//
// Cortex never stores one. It is a projection of a run, rebuilt on every
// read, so the task and the run cannot drift apart and claim different
// things about the same piece of work.
type Task struct {
	ID        string         `json:"id"`
	ContextID string         `json:"contextId,omitempty"`
	Status    TaskStatus     `json:"status"`
	Artifacts []Artifact     `json:"artifacts,omitempty"`
	History   []Message      `json:"history,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// SendMessageRequest is what a peer sends to reach an agent.
//
// Tenant is the protocol's routing identifier for serving many agents
// behind one endpoint, and in cortex it is the agent's name. SenderName
// is a cortex addition carried in request metadata: a peer may say which
// of ITS agents is speaking, and the service namespaces that name under
// the node the resolver assigned, so it can never read as a local agent.
type SendMessageRequest struct {
	Tenant     string         `json:"tenant,omitempty"`
	Message    Message        `json:"message"`
	SenderName string         `json:"senderName,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

// SendMessageResult is either an acknowledgement or the task that the
// message started. Exactly one is set.
type SendMessageResult struct {
	Message *Message `json:"message,omitempty"`
	Task    *Task    `json:"task,omitempty"`
}

// GetTaskRequest addresses one task.
type GetTaskRequest struct {
	Tenant        string `json:"tenant,omitempty"`
	ID            string `json:"id"`
	HistoryLength int    `json:"historyLength,omitempty"`
}

// ListTasksRequest pages through an agent's tasks.
type ListTasksRequest struct {
	Tenant   string `json:"tenant,omitempty"`
	PageSize int    `json:"pageSize,omitempty"`
}

// ListTasksResult is a page of tasks.
type ListTasksResult struct {
	Tasks []Task `json:"tasks"`
}

// CancelTaskRequest asks an agent to stop.
type CancelTaskRequest struct {
	Tenant string `json:"tenant,omitempty"`
	ID     string `json:"id"`
}

func timestamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }
