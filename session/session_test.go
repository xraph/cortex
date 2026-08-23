package session_test

import (
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/session"
)

func TestSession_CarriesScope(t *testing.T) {
	s := session.Session{
		ID:    id.NewSessionID(),
		Scope: cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}}},
	}
	if s.ID.IsNil() {
		t.Fatal("session id should not be nil")
	}
	if got := s.Scope.Canonical(); got != "workspace=ws_x" {
		t.Errorf("scope = %q, want workspace=ws_x", got)
	}
}

func TestSessionID_HasItsOwnPrefix(t *testing.T) {
	sid := id.NewSessionID()
	if _, err := id.ParseSessionID(sid.String()); err != nil {
		t.Errorf("a freshly minted session id must parse as one: %v", err)
	}
	// A persona id must NOT parse as a session id, or the prefix is doing
	// nothing and a caller could pass the wrong entity's id anywhere.
	if _, err := id.ParseSessionID(id.NewPersonaID().String()); err == nil {
		t.Error("a persona id parsed as a session id; the prefix is not discriminating")
	}
}
