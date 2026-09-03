package a2aremote

import (
	"fmt"
	"strings"
)

// The A2A error codes, from the protocol's own error mapping table.
// A2A-specific errors occupy -32001 to -32099; everything at -32600 and
// below is standard JSON-RPC.
const (
	CodeTaskNotFound                 = -32001
	CodeTaskNotCancelable            = -32002
	CodePushNotificationNotSupported = -32003
	CodeUnsupportedOperation         = -32004
	CodeContentTypeNotSupported      = -32005
	CodeInvalidAgentResponse         = -32006
	CodeExtendedCardNotConfigured    = -32007
	CodeExtensionSupportRequired     = -32008
	CodeVersionNotSupported          = -32009

	CodeParse          = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603
)

// Error is one protocol error. It carries the numeric code every binding
// maps to its own representation, so a service method can raise the
// right thing without knowing which binding is carrying it.
type Error struct {
	Code    int              `json:"code"`
	Message string           `json:"message"`
	Data    []map[string]any `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("a2a error %d: %s", e.Code, e.Message) }

func newError(code int, msg string) *Error { return &Error{Code: code, Message: msg} }

// ErrTaskNotFound is also what an unknown tenant returns, deliberately.
// A distinct "no such agent" would let a caller enumerate which agents
// exist here by probing names.
func ErrTaskNotFound(id string) *Error {
	return newError(CodeTaskNotFound, fmt.Sprintf("no task %q", id))
}

func ErrTaskNotCancelable(id string) *Error {
	return newError(CodeTaskNotCancelable, fmt.Sprintf("task %q has already finished", id))
}

func ErrPushNotificationNotSupported() *Error {
	return newError(CodePushNotificationNotSupported, "this agent does not support push notifications")
}

func ErrUnsupportedOperation(method string) *Error {
	return newError(CodeUnsupportedOperation, fmt.Sprintf("%s is not supported by this agent", method))
}

func ErrContentTypeNotSupported(kind string) *Error {
	return newError(CodeContentTypeNotSupported, fmt.Sprintf("%s parts are not supported: send text", kind))
}

func ErrInvalidAgentResponse(why string) *Error {
	return newError(CodeInvalidAgentResponse, "the agent answered with something unusable: "+why)
}

func ErrExtendedCardNotConfigured() *Error {
	return newError(CodeExtendedCardNotConfigured, "no extended agent card is configured")
}

func ErrExtensionSupportRequired(uri string) *Error {
	return newError(CodeExtensionSupportRequired, fmt.Sprintf("extension %q is required and not supported here", uri))
}

func ErrVersionNotSupported(v string) *Error {
	return newError(CodeVersionNotSupported, fmt.Sprintf("protocol version %q is not supported", v))
}

func ErrParse() *Error { return newError(CodeParse, "the request body is not valid JSON") }

func ErrInvalidRequest(why string) *Error { return newError(CodeInvalidRequest, why) }

func ErrMethodNotFound(method string) *Error {
	return newError(CodeMethodNotFound, fmt.Sprintf("unknown method %q", method))
}

func ErrInvalidParams(why string) *Error { return newError(CodeInvalidParams, why) }

func ErrInternal(why string) *Error { return newError(CodeInternal, why) }

// ErrUnauthenticated says only that the caller was refused.
//
// The dullness is the point: a caller that has not proved who it is
// learns nothing about what exists here, which agent names are real, or
// what it would have needed to present.
func ErrUnauthenticated() *Error {
	return newError(CodeInvalidRequest, "request refused")
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}
