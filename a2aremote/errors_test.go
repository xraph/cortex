package a2aremote

import (
	"errors"
	"testing"
)

// The numeric codes are the protocol's, from its own error mapping
// table. A wrong number is a peer that cannot tell "no such task" from
// "you are not allowed", so they are pinned rather than remembered.
func TestErrorCodes(t *testing.T) {
	cases := []struct {
		err  *Error
		code int
		name string
	}{
		{ErrTaskNotFound("arun_1"), -32001, "TaskNotFoundError"},
		{ErrTaskNotCancelable("arun_1"), -32002, "TaskNotCancelableError"},
		{ErrPushNotificationNotSupported(), -32003, "PushNotificationNotSupportedError"},
		{ErrUnsupportedOperation("GetExtendedAgentCard"), -32004, "UnsupportedOperationError"},
		{ErrContentTypeNotSupported("file"), -32005, "ContentTypeNotSupportedError"},
		{ErrInvalidAgentResponse("no parts"), -32006, "InvalidAgentResponseError"},
		{ErrExtendedCardNotConfigured(), -32007, "ExtendedAgentCardNotConfiguredError"},
		{ErrExtensionSupportRequired("urn:x"), -32008, "ExtensionSupportRequiredError"},
		{ErrVersionNotSupported("0.1"), -32009, "VersionNotSupportedError"},
	}
	for _, tc := range cases {
		if tc.err.Code != tc.code {
			t.Errorf("%s: code = %d, want %d", tc.name, tc.err.Code, tc.code)
		}
		if tc.err.Message == "" {
			t.Errorf("%s: a peer debugging an integration reads this string, so it must say something", tc.name)
		}
	}
}

func TestStandardJSONRPCCodes(t *testing.T) {
	if ErrParse().Code != -32700 {
		t.Errorf("parse error = %d, want -32700", ErrParse().Code)
	}
	if ErrInvalidRequest("x").Code != -32600 {
		t.Errorf("invalid request = %d, want -32600", ErrInvalidRequest("x").Code)
	}
	if ErrMethodNotFound("Nope").Code != -32601 {
		t.Errorf("method not found = %d, want -32601", ErrMethodNotFound("Nope").Code)
	}
	if ErrInvalidParams("x").Code != -32602 {
		t.Errorf("invalid params = %d, want -32602", ErrInvalidParams("x").Code)
	}
	if ErrInternal("x").Code != -32603 {
		t.Errorf("internal = %d, want -32603", ErrInternal("x").Code)
	}
}

// An unauthenticated caller must learn that it was refused and nothing
// else about what exists here, so the message is deliberately dull.
func TestUnauthenticatedSaysNothingUseful(t *testing.T) {
	err := ErrUnauthenticated()
	if err.Code != -32600 {
		t.Errorf("code = %d, want -32600", err.Code)
	}
	for _, leak := range []string{"scope", "tenant", "agent", "peer"} {
		if containsFold(err.Message, leak) {
			t.Errorf("the refusal message leaks %q: %q", leak, err.Message)
		}
	}
}

func TestErrorIsAnError(t *testing.T) {
	var err error = ErrTaskNotFound("arun_1")
	var target *Error
	if !errors.As(err, &target) {
		t.Fatal("*Error must satisfy error and be recoverable with errors.As")
	}
	if target.Error() == "" {
		t.Fatal("Error() must render something")
	}
}
