package a2a

import "testing"

// The table is exhaustive on purpose. A performative added later without a
// routing class fails here rather than silently defaulting to the inbox.
func TestPerformativeClass(t *testing.T) {
	cases := map[Performative]Class{
		Request:         ClassDirective,
		RequestWhen:     ClassDirective,
		RequestWhenever: ClassDirective,
		QueryIf:         ClassDirective,
		QueryRef:        ClassDirective,
		CFP:             ClassDirective,
		Propose:         ClassDirective,
		AcceptProposal:  ClassDirective,

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
		Proxy:          ClassInformative,
		Propagate:      ClassInformative,

		Cancel: ClassControl,
	}

	if len(cases) != 22 {
		t.Fatalf("table covers %d performatives, FIPA-ACL defines 22", len(cases))
	}
	for p, want := range cases {
		got, ok := p.Class()
		if !ok {
			t.Errorf("%s: not classified", p)
			continue
		}
		if got != want {
			t.Errorf("%s: class = %s, want %s", p, got, want)
		}
	}
}

func TestAllPerformativesAreClassified(t *testing.T) {
	all := AllPerformatives()
	for _, p := range all {
		if _, ok := p.Class(); !ok {
			t.Errorf("%s has no routing class", p)
		}
	}
	if len(all) != 22 {
		t.Fatalf("AllPerformatives returned %d, want 22", len(all))
	}
}

func TestUnknownPerformativeIsNotClassified(t *testing.T) {
	if _, ok := Performative("shout").Class(); ok {
		t.Fatal("an invented performative must not classify")
	}
	if Performative("shout").Valid() {
		t.Fatal("an invented performative must not validate")
	}
}

// ResolvesAsk is the pair most likely to be got backwards: agree means the
// peer took the job and is still working, so it must not un-pause the asker.
func TestAnswersAsk(t *testing.T) {
	// propose is an answer to a call for proposals, and refusing is an
	// answer too: a participant that will not bid has told the initiator
	// what it needed to know about that participant.
	answering := []Performative{
		Inform, InformIf, InformRef, Confirm, Disconfirm,
		Refuse, Failure, NotUnderstood, RejectProposal, Propose,
	}
	for _, p := range answering {
		if !p.AnswersAsk() {
			t.Errorf("%s should count as an answer", p)
		}
	}
	// agree means "working on it", so counting it would resume an asker
	// on a message carrying no answer.
	for _, p := range []Performative{Agree, Subscribe, Request, CFP} {
		if p.AnswersAsk() {
			t.Errorf("%s must not count as an answer", p)
		}
	}
}
