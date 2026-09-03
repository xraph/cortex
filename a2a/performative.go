// Package a2a gives cortex agents direct, addressable communication:
// FIPA-ACL messages, durable conversations, mailboxes, and an ask that
// suspends the sender's run until a peer answers.
//
// It is a leaf package. It depends only on cortex and id, and reaches host
// capability (running an agent, resuming a paused run, persistence,
// delivery) through injected interfaces, never by importing the engine.
package a2a

// Performative is the speech act a message performs. The 22 constants
// below are the complete FIPA-ACL set.
type Performative string

// The FIPA-ACL performatives.
const (
	AcceptProposal  Performative = "accept-proposal"
	Agree           Performative = "agree"
	Cancel          Performative = "cancel"
	CFP             Performative = "cfp"
	Confirm         Performative = "confirm"
	Disconfirm      Performative = "disconfirm"
	Failure         Performative = "failure"
	Inform          Performative = "inform"
	InformIf        Performative = "inform-if"
	InformRef       Performative = "inform-ref"
	NotUnderstood   Performative = "not-understood"
	Propagate       Performative = "propagate"
	Propose         Performative = "propose"
	Proxy           Performative = "proxy"
	QueryIf         Performative = "query-if"
	QueryRef        Performative = "query-ref"
	Refuse          Performative = "refuse"
	RejectProposal  Performative = "reject-proposal"
	Request         Performative = "request"
	RequestWhen     Performative = "request-when"
	RequestWhenever Performative = "request-whenever"
	Subscribe       Performative = "subscribe"
)

// Class is how cortex routes a performative on arrival.
type Class string

// The three routing classes.
const (
	// ClassDirective starts a run for the recipient; its output is the reply.
	ClassDirective Class = "directive"
	// ClassInformative lands in the recipient's inbox and starts nothing.
	ClassInformative Class = "informative"
	// ClassControl is interpreted by the bus itself and reaches no agent.
	ClassControl Class = "control"
)

// classes maps every performative to its routing class. A performative
// missing from this map is not deliverable.
var classes = map[Performative]Class{
	Request:         ClassDirective,
	RequestWhen:     ClassDirective,
	RequestWhenever: ClassDirective,
	QueryIf:         ClassDirective,
	QueryRef:        ClassDirective,
	CFP:             ClassDirective,
	Propose:         ClassDirective,
	// accept-proposal is a directive, not an informative: in Contract Net
	// it is the message that makes the contractor do the work.
	AcceptProposal: ClassDirective,

	Inform:         ClassInformative,
	InformIf:       ClassInformative,
	InformRef:      ClassInformative,
	Confirm:        ClassInformative,
	Disconfirm:     ClassInformative,
	Agree:          ClassInformative,
	Refuse:         ClassInformative,
	Failure:        ClassInformative,
	NotUnderstood:  ClassInformative,
	RejectProposal: ClassInformative,
	Subscribe:      ClassInformative,
	// proxy and propagate are carried and delivered, but cortex does not
	// forward them on an agent's behalf. A host that wants forwarding
	// builds it over the inbox.
	Proxy:     ClassInformative,
	Propagate: ClassInformative,

	Cancel: ClassControl,
}

// Class returns the routing class for p, and whether p is a performative
// cortex recognises at all.
func (p Performative) Class() (Class, bool) {
	c, ok := classes[p]
	return c, ok
}

// Valid reports whether p is one of the 22 FIPA-ACL performatives.
func (p Performative) Valid() bool {
	_, ok := classes[p]
	return ok
}

// AnswersAsk reports whether a reply carrying p counts as an answer to a
// waiting ask.
//
// agree is deliberately excluded. It means the peer accepted the task and
// is still working on it, so an asker that counted it would resume on a
// message carrying no answer.
//
// refuse and reject-proposal DO count. Declining is an answer: in a
// tender, a participant that will not bid has told the initiator what it
// needed to know about that participant.
func (p Performative) AnswersAsk() bool {
	switch p {
	case Inform, InformIf, InformRef, Confirm, Disconfirm,
		Refuse, Failure, NotUnderstood, RejectProposal, Propose:
		return true
	default:
		return false
	}
}

// AllPerformatives returns every recognised performative, for validation
// and for exhaustive tests.
func AllPerformatives() []Performative {
	out := make([]Performative, 0, len(classes))
	for p := range classes {
		out = append(out, p)
	}
	return out
}
