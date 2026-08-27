package a2aremote

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

// fipaMeta is the ACL parameter set as it travels in Message.metadata,
// under FIPAExtensionURI as its key.
//
// conversationId is deliberately absent: it is contextId, a native A2A
// field, and carrying it in both places would create two things to
// disagree. Sender is absent for a harder reason: a peer does not get to
// name who is speaking. That comes from the credentials it presented.
type fipaMeta struct {
	Performative string `json:"performative,omitempty"`
	ReplyWith    string `json:"replyWith,omitempty"`
	InReplyTo    string `json:"inReplyTo,omitempty"`
	Ontology     string `json:"ontology,omitempty"`
	Protocol     string `json:"protocol,omitempty"`
	Language     string `json:"language,omitempty"`
	Encoding     string `json:"encoding,omitempty"`
	ReplyBy      string `json:"replyBy,omitempty"`
}

// MessageFromEnvelope renders a cortex envelope as an A2A message.
//
// The ACL parameters ride in metadata under the extension's URI, and the
// message declares that URI in its extensions list, which is what the
// protocol asks of a message an extension contributed to.
func MessageFromEnvelope(e *a2a.Envelope) Message {
	meta := fipaMeta{
		Performative: string(e.Performative),
		ReplyWith:    e.ReplyWith,
		InReplyTo:    e.InReplyTo,
		Ontology:     e.Ontology,
		Protocol:     e.Protocol,
		Language:     e.Language,
		Encoding:     e.Encoding,
	}
	if e.ReplyBy != nil {
		meta.ReplyBy = timestamp(*e.ReplyBy)
	}

	m := Message{
		MessageID:  e.ID.String(),
		Role:       RoleAgent,
		Parts:      []Part{{Text: e.Content}},
		Extensions: []string{FIPAExtensionURI},
		Metadata:   map[string]any{FIPAExtensionURI: meta},
	}
	if !e.ConversationID.IsNil() {
		m.ContextID = e.ConversationID.String()
	}
	if !e.OriginRunID.IsNil() {
		m.TaskID = e.OriginRunID.String()
	}
	return m
}

// EnvelopeParamsFromMessage turns an inbound A2A message into the send
// params the bus takes.
//
// sender and receiver are arguments rather than anything read out of the
// message, and that is the security property this signature exists to
// enforce: the caller passes the sender its credentials earned, so no
// field a peer controls can change who the message appears to be from.
func EnvelopeParamsFromMessage(m Message, sender, receiver a2a.Address) (a2a.SendParams, error) {
	content, err := contentOf(m.Parts)
	if err != nil {
		return a2a.SendParams{}, err
	}

	meta := fipaFrom(m.Metadata)
	perf := a2a.Performative(meta.Performative)
	if perf == "" {
		// A peer that knows nothing about FIPA is asking for something,
		// which is what a request means. Reading it as an inform instead
		// would file the message in a mailbox nobody is watching.
		perf = a2a.Request
	}

	params := a2a.SendParams{
		Sender:       sender,
		Receivers:    []a2a.Address{receiver},
		Performative: perf,
		Content:      content,
		Ontology:     meta.Ontology,
		Protocol:     meta.Protocol,
		Language:     meta.Language,
		Encoding:     meta.Encoding,
		ReplyWith:    meta.ReplyWith,
		InReplyTo:    meta.InReplyTo,
	}
	if m.ContextID != "" {
		convID, convErr := id.ParseWithPrefix(m.ContextID, id.PrefixConversation)
		if convErr != nil {
			return a2a.SendParams{}, ErrInvalidParams("contextId is not a conversation id: " + convErr.Error())
		}
		params.ConversationID = convID
	}
	if meta.ReplyBy != "" {
		by, byErr := time.Parse(time.RFC3339, meta.ReplyBy)
		if byErr != nil {
			return a2a.SendParams{}, ErrInvalidParams("replyBy is not an RFC3339 timestamp")
		}
		params.ReplyBy = &by
	}
	return params, nil
}

// contentOf flattens the parts into text, refusing the shapes cortex
// cannot read rather than dropping them. A peer whose attachment
// vanished silently has no way to work out why the answer made no sense.
func contentOf(parts []Part) (string, error) {
	var texts []string
	for _, p := range parts {
		switch {
		case p.File != nil:
			return "", ErrContentTypeNotSupported("file")
		case p.Data != nil:
			return "", ErrContentTypeNotSupported("data")
		case p.Text != "":
			texts = append(texts, p.Text)
		}
	}
	if len(texts) == 0 {
		return "", ErrInvalidParams("the message carries no text to act on")
	}
	return strings.Join(texts, "\n\n"), nil
}

// fipaFrom pulls the ACL parameters out of a message's metadata. A
// message without them decodes to the zero value, which is what makes a
// FIPA-unaware peer work.
func fipaFrom(metadata map[string]any) fipaMeta {
	raw, ok := metadata[FIPAExtensionURI]
	if !ok {
		return fipaMeta{}
	}
	// The value arrives as a decoded any, so it is round-tripped rather
	// than type-asserted field by field.
	encoded, err := json.Marshal(raw)
	if err != nil {
		return fipaMeta{}
	}
	var meta fipaMeta
	if err := json.Unmarshal(encoded, &meta); err != nil {
		return fipaMeta{}
	}
	return meta
}

// TaskFromRun projects a run as a task.
//
// Nothing is stored. A task rebuilt on every read cannot drift from the
// run it describes, which is the failure mode a stored task table would
// have made possible.
func TaskFromRun(r *run.Run, contextID string) Task {
	state := taskState(r.State)
	task := Task{
		ID:        r.ID.String(),
		ContextID: contextID,
		Status:    TaskStatus{State: state, Timestamp: timestamp(time.Now())},
	}

	switch {
	case state == TaskStateCompleted && r.Output != "":
		task.Artifacts = []Artifact{{
			ArtifactID: r.ID.String() + "-output",
			Name:       "output",
			Parts:      []Part{{Text: r.Output}},
		}}
	case r.Error != "":
		task.Status.Message = &Message{
			MessageID: r.ID.String() + "-status",
			Role:      RoleAgent,
			Parts:     []Part{{Text: r.Error}},
		}
	}
	return task
}

func taskState(s run.State) TaskState {
	switch s {
	case run.StateCreated:
		return TaskStateSubmitted
	case run.StateRunning:
		return TaskStateWorking
	case run.StateCompleted:
		return TaskStateCompleted
	case run.StateFailed:
		return TaskStateFailed
	case run.StateCancelled:
		return TaskStateCanceled
	case run.StatePaused:
		// Waiting on something outside itself, which is what the state
		// means. Whether that is a human approving, a host executing a
		// tool, or another agent answering is not the peer's business.
		return TaskStateInputRequired
	default:
		return TaskStateSubmitted
	}
}
