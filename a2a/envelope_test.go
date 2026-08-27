package a2a

import (
	"errors"
	"testing"
)

func TestEnvelopeValidate(t *testing.T) {
	base := func() *Envelope {
		return &Envelope{
			Performative: Request,
			Sender:       Address{Agent: "planner"},
			Receivers:    []Address{{Agent: "worker"}},
			Content:      "do the thing",
		}
	}

	t.Run("valid", func(t *testing.T) {
		if err := base().Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("unknown performative", func(t *testing.T) {
		e := base()
		e.Performative = "shout"
		if !errors.Is(e.Validate(), ErrInvalidPerformative) {
			t.Fatal("want ErrInvalidPerformative")
		}
	})

	t.Run("no receivers", func(t *testing.T) {
		e := base()
		e.Receivers = nil
		if !errors.Is(e.Validate(), ErrNoReceivers) {
			t.Fatal("want ErrNoReceivers")
		}
	})

	t.Run("no sender", func(t *testing.T) {
		e := base()
		e.Sender = Address{}
		if !errors.Is(e.Validate(), ErrNoSender) {
			t.Fatal("want ErrNoSender")
		}
	})

	// Self-addressing is refused outright. The loop risk is real and the
	// use case is not.
	t.Run("self addressed", func(t *testing.T) {
		e := base()
		e.Receivers = []Address{{Agent: "planner"}}
		if !errors.Is(e.Validate(), ErrSelfAddressed) {
			t.Fatal("want ErrSelfAddressed")
		}
	})

	t.Run("self addressed among others", func(t *testing.T) {
		e := base()
		e.Receivers = []Address{{Agent: "worker"}, {Agent: "planner"}}
		if !errors.Is(e.Validate(), ErrSelfAddressed) {
			t.Fatal("a broadcast that includes the sender is still self-addressed")
		}
	})

	t.Run("receiver with empty agent", func(t *testing.T) {
		e := base()
		e.Receivers = []Address{{Agent: ""}}
		if !errors.Is(e.Validate(), ErrNoReceivers) {
			t.Fatal("want ErrNoReceivers")
		}
	})
}

func TestAddressIsLocal(t *testing.T) {
	if !(Address{Agent: "a"}).IsLocal() {
		t.Fatal("an empty Node means an agent in this engine")
	}
	if (Address{Agent: "a", Node: "peer.example"}).IsLocal() {
		t.Fatal("a Node means a remote peer")
	}
}

func TestAddressEqualComparesTheNodeToo(t *testing.T) {
	a := Address{Agent: "x"}
	if a.Equal(Address{Agent: "x", Node: "n"}) {
		t.Fatal("same agent name on a different node is a different address")
	}
	if !a.Equal(Address{Agent: "x"}) {
		t.Fatal("identical addresses must compare equal")
	}
}

func TestAddressString(t *testing.T) {
	if got := (Address{Agent: "x"}).String(); got != "x" {
		t.Fatalf("String() = %q, want %q", got, "x")
	}
	if got := (Address{Agent: "x", Node: "n"}).String(); got != "x@n" {
		t.Fatalf("String() = %q, want %q", got, "x@n")
	}
}

// Agents write addresses as text, and "worker@peer.example" has to mean
// the remote worker rather than a local agent whose name contains an @.
func TestParseAddress(t *testing.T) {
	cases := map[string]Address{
		"worker":              {Agent: "worker"},
		"worker@peer.example": {Agent: "worker", Node: "peer.example"},
		"  worker  ":          {Agent: "worker"},
		"worker@":             {Agent: "worker"},
		"":                    {},
	}
	for in, want := range cases {
		if got := ParseAddress(in); got != want {
			t.Errorf("ParseAddress(%q) = %+v, want %+v", in, got, want)
		}
	}
}

// A node containing an @ would be ambiguous, so the split is on the
// LAST one: an agent name cannot contain an @, but a node might.
func TestParseAddressSplitsOnTheLastAt(t *testing.T) {
	got := ParseAddress("worker@a@b.example")
	if got.Agent != "worker@a" || got.Node != "b.example" {
		t.Fatalf("got %+v", got)
	}
}

func TestAddressStringRoundTrips(t *testing.T) {
	for _, in := range []string{"worker", "worker@peer.example"} {
		if got := ParseAddress(in).String(); got != in {
			t.Errorf("round trip of %q gave %q", in, got)
		}
	}
}
