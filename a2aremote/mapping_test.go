package a2aremote

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

func TestTaskStateProjection(t *testing.T) {
	cases := map[run.State]TaskState{
		run.StateCreated:   TaskStateSubmitted,
		run.StateRunning:   TaskStateWorking,
		run.StateCompleted: TaskStateCompleted,
		run.StateFailed:    TaskStateFailed,
		run.StateCancelled: TaskStateCanceled,
		// A paused run is waiting on something outside itself, which is
		// exactly what INPUT_REQUIRED means. The peer learns that it is
		// waiting without learning whose business it waits on.
		run.StatePaused: TaskStateInputRequired,
	}
	for state, want := range cases {
		if got := taskState(state); got != want {
			t.Errorf("%s -> %s, want %s", state, got, want)
		}
	}
}

func TestEnvelopeRoundTripsThroughAMessage(t *testing.T) {
	deadline := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	e := &a2a.Envelope{
		ID: id.NewMessageID(), ConversationID: id.NewConversationID(),
		Performative: a2a.CFP, Sender: a2a.Address{Agent: "planner"},
		Receivers: []a2a.Address{{Agent: "worker", Node: "peer.example"}},
		Content:   "who can take this?", Ontology: "ops", Protocol: "fipa-contract-net",
		ReplyWith: "rw-1", ReplyBy: &deadline,
	}
	m := MessageFromEnvelope(e)

	if m.ContextID != e.ConversationID.String() {
		t.Errorf("contextId = %q, want the conversation id", m.ContextID)
	}
	if len(m.Parts) != 1 || m.Parts[0].Text != "who can take this?" {
		t.Errorf("parts lost the content: %+v", m.Parts)
	}
	if len(m.Extensions) != 1 || m.Extensions[0] != FIPAExtensionURI {
		t.Errorf("a message using the extension must declare it: %+v", m.Extensions)
	}

	params, err := EnvelopeParamsFromMessage(m,
		a2a.Address{Agent: "planner", Node: "peer.example"},
		a2a.Address{Agent: "worker"})
	if err != nil {
		t.Fatalf("EnvelopeParamsFromMessage: %v", err)
	}
	if params.Performative != a2a.CFP {
		t.Errorf("performative = %s, want cfp", params.Performative)
	}
	if params.Ontology != "ops" || params.Protocol != "fipa-contract-net" || params.ReplyWith != "rw-1" {
		t.Errorf("ACL parameters did not survive: %+v", params)
	}
	if params.ReplyBy == nil || !params.ReplyBy.Equal(deadline) {
		t.Errorf("replyBy did not survive: %v", params.ReplyBy)
	}
	if params.ConversationID != e.ConversationID {
		t.Errorf("the conversation did not survive as contextId: %v", params.ConversationID)
	}
}

// A peer that has never heard of FIPA still has to work. This is what
// keeps the extension optional in practice and not only in the card.
func TestMessageWithoutFIPAMetadataGetsASensibleDefault(t *testing.T) {
	plain := Message{MessageID: "m1", Role: RoleUser, Parts: []Part{{Text: "do the thing"}}}

	params, err := EnvelopeParamsFromMessage(plain,
		a2a.Address{Agent: "someone", Node: "peer.example"},
		a2a.Address{Agent: "worker"})
	if err != nil {
		t.Fatalf("EnvelopeParamsFromMessage: %v", err)
	}
	if params.Performative != a2a.Request {
		t.Fatalf("performative = %s, want request for a plain inbound message", params.Performative)
	}
	if params.Content != "do the thing" {
		t.Fatalf("content = %q", params.Content)
	}
}

func TestSenderAlwaysComesFromTheArgument(t *testing.T) {
	// The message tries to name a sender of its own in every field a
	// peer could reach. None of them may win.
	m := Message{
		MessageID: "m1", Role: RoleUser, Parts: []Part{{Text: "trust me"}},
		Metadata: map[string]any{
			"sender": "planner",
			"from":   "planner",
			FIPAExtensionURI: map[string]any{
				"performative": "inform",
				"sender":       "planner",
			},
		},
	}
	params, err := EnvelopeParamsFromMessage(m,
		a2a.Address{Agent: "their-agent", Node: "peer.example"},
		a2a.Address{Agent: "worker"})
	if err != nil {
		t.Fatalf("EnvelopeParamsFromMessage: %v", err)
	}
	if params.Sender.Agent != "their-agent" || params.Sender.Node != "peer.example" {
		t.Fatalf("sender = %+v, want the one the caller passed", params.Sender)
	}
}

func TestMessageWithSeveralTextPartsIsJoined(t *testing.T) {
	m := Message{MessageID: "m1", Role: RoleUser, Parts: []Part{{Text: "first"}, {Text: "second"}}}
	params, err := EnvelopeParamsFromMessage(m, a2a.Address{Agent: "s", Node: "n"}, a2a.Address{Agent: "w"})
	if err != nil {
		t.Fatalf("EnvelopeParamsFromMessage: %v", err)
	}
	if params.Content != "first\n\nsecond" {
		t.Fatalf("content = %q, want the parts joined", params.Content)
	}
}

func TestUnsupportedPartTypeIsRefused(t *testing.T) {
	for name, m := range map[string]Message{
		"file": {MessageID: "m1", Role: RoleUser, Parts: []Part{{File: &FilePart{URL: "https://x/y.png"}}}},
		"data": {MessageID: "m1", Role: RoleUser, Parts: []Part{{Data: &DataPart{Data: map[string]any{"k": "v"}}}}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := EnvelopeParamsFromMessage(m, a2a.Address{Agent: "s", Node: "n"}, a2a.Address{Agent: "w"})
			var aerr *Error
			if !errors.As(err, &aerr) || aerr.Code != CodeContentTypeNotSupported {
				t.Fatalf("err = %v, want ContentTypeNotSupportedError", err)
			}
		})
	}
}

func TestEmptyMessageIsRefused(t *testing.T) {
	_, err := EnvelopeParamsFromMessage(Message{MessageID: "m1", Role: RoleUser},
		a2a.Address{Agent: "s", Node: "n"}, a2a.Address{Agent: "w"})
	var aerr *Error
	if !errors.As(err, &aerr) || aerr.Code != CodeInvalidParams {
		t.Fatalf("err = %v, want InvalidParams", err)
	}
}

func TestTaskFromRun(t *testing.T) {
	r := &run.Run{ID: id.NewAgentRunID(), State: run.StateCompleted, Output: "done and dusted"}
	task := TaskFromRun(r, "conv_1")

	if task.ID != r.ID.String() {
		t.Errorf("task id = %q, want the run id verbatim", task.ID)
	}
	if task.ContextID != "conv_1" {
		t.Errorf("contextId = %q", task.ContextID)
	}
	if task.Status.State != TaskStateCompleted {
		t.Errorf("state = %s", task.Status.State)
	}
	if len(task.Artifacts) != 1 || task.Artifacts[0].Parts[0].Text != "done and dusted" {
		t.Errorf("the run output must arrive as an artifact: %+v", task.Artifacts)
	}
}

func TestFailedRunCarriesItsErrorIntoTheStatus(t *testing.T) {
	r := &run.Run{ID: id.NewAgentRunID(), State: run.StateFailed, Error: "the model refused"}
	task := TaskFromRun(r, "")

	if task.Status.State != TaskStateFailed {
		t.Fatalf("state = %s, want failed", task.Status.State)
	}
	if task.Status.Message == nil || !strings.Contains(task.Status.Message.Parts[0].Text, "the model refused") {
		t.Fatalf("a failed task must say why: %+v", task.Status)
	}
	if len(task.Artifacts) != 0 {
		t.Fatalf("a failed run produced no output, so it has no artifacts: %+v", task.Artifacts)
	}
}

func TestRunningRunHasNoArtifactsYet(t *testing.T) {
	r := &run.Run{ID: id.NewAgentRunID(), State: run.StateRunning}
	task := TaskFromRun(r, "")
	if task.Status.State != TaskStateWorking || len(task.Artifacts) != 0 {
		t.Fatalf("task = %+v", task)
	}
}

// A peer's message id is the peer's business. Ours are TypeIDs; a
// perfectly conformant A2A implementation might use UUIDs, or a
// sequence, or anything else, and a message that quotes one of those in
// inReplyTo is still a valid message.
//
// So an unrecognised token drops the correlation rather than the
// message. The bus already treats an in-reply-to that matches no
// pending ask as ordinary mail, which is the same outcome by a different
// route, and refusing the whole request instead would make cortex
// unable to talk to any peer whose ids are not shaped like ours.
func TestAnUnrecognisedInReplyToDropsTheCorrelationNotTheMessage(t *testing.T) {
	m := Message{
		MessageID: "not-a-typeid-either", Role: RoleUser, Parts: []Part{{Text: "answering you"}},
		Metadata: map[string]any{FIPAExtensionURI: map[string]any{
			"performative": "inform",
			"inReplyTo":    "9f8b2c1e-4a7d-11ef-9c3d-0242ac120002",
		}},
	}

	params, err := EnvelopeParamsFromMessage(m, a2a.Address{Agent: "them", Node: "peer.example"}, a2a.Address{Agent: "worker"})
	if err != nil {
		t.Fatalf("a valid A2A message was refused over a token shape: %v", err)
	}
	if params.Content != "answering you" {
		t.Fatalf("content = %q", params.Content)
	}
	if params.InReplyTo != "" {
		t.Fatalf("InReplyTo = %q, want it dropped: it can never match a token we minted", params.InReplyTo)
	}
}

// A token we minted correlates, which is the case that matters: it is
// the only shape a reply to one of our own asks can carry.
func TestOurOwnInReplyToSurvives(t *testing.T) {
	ours := id.NewMessageID().String()
	m := Message{
		MessageID: "m1", Role: RoleUser, Parts: []Part{{Text: "answering you"}},
		Metadata: map[string]any{FIPAExtensionURI: map[string]any{
			"performative": "inform",
			"inReplyTo":    ours,
		}},
	}

	params, err := EnvelopeParamsFromMessage(m, a2a.Address{Agent: "them", Node: "peer.example"}, a2a.Address{Agent: "worker"})
	if err != nil {
		t.Fatalf("EnvelopeParamsFromMessage: %v", err)
	}
	if params.InReplyTo != ours {
		t.Fatalf("InReplyTo = %q, want %q", params.InReplyTo, ours)
	}
}
