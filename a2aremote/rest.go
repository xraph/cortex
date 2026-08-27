package a2aremote

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// RESTHandler serves the HTTP+JSON binding.
//
// The paths are the protocol's own, from the http annotations on the
// normative proto: a colon verb like message:send is unusual in Go
// routing and entirely ordinary in an AIP-style API, so they are spelled
// the way a client expects rather than the way a Go router prefers.
//
// Like the JSON-RPC handler, this decodes, dispatches and encodes.
// Nothing here decides what an operation means.
func (s *Service) RESTHandler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /{tenant}/message:send", func(w http.ResponseWriter, r *http.Request) {
		var req SendMessageRequest
		if !decodeREST(w, r, &req) {
			return
		}
		req.Tenant = r.PathValue("tenant")
		result, err := s.SendMessage(r.Context(), credentialsOf(r), req)
		writeREST(w, result, err)
	})

	mux.HandleFunc("GET /{tenant}/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		// A colon verb rides on the id segment, so the two forms are
		// told apart here rather than by the router.
		id, verb := splitVerb(r.PathValue("id"))
		switch verb {
		case "":
			task, err := s.GetTask(r.Context(), credentialsOf(r), GetTaskRequest{
				Tenant: r.PathValue("tenant"), ID: id,
			})
			writeREST(w, task, err)
		case "subscribe":
			writeREST(w, nil, ErrUnsupportedOperation("SubscribeToTask"))
		default:
			writeREST(w, nil, ErrMethodNotFound(verb))
		}
	})

	mux.HandleFunc("GET /{tenant}/tasks", func(w http.ResponseWriter, r *http.Request) {
		result, err := s.ListTasks(r.Context(), credentialsOf(r), ListTasksRequest{
			Tenant: r.PathValue("tenant"),
		})
		writeREST(w, result, err)
	})

	mux.HandleFunc("POST /{tenant}/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, verb := splitVerb(r.PathValue("id"))
		if verb != "cancel" {
			writeREST(w, nil, ErrMethodNotFound(verb))
			return
		}
		task, err := s.CancelTask(r.Context(), credentialsOf(r), CancelTaskRequest{
			Tenant: r.PathValue("tenant"), ID: id,
		})
		writeREST(w, task, err)
	})

	mux.HandleFunc("POST /{tenant}/message:stream", func(w http.ResponseWriter, _ *http.Request) {
		writeREST(w, nil, ErrUnsupportedOperation("SendStreamingMessage"))
	})
	mux.HandleFunc("GET /{tenant}/extendedAgentCard", func(w http.ResponseWriter, _ *http.Request) {
		writeREST(w, nil, ErrExtendedCardNotConfigured())
	})
	// Push notification config, in all four spellings, refused in one
	// place rather than four.
	for _, pattern := range []string{
		"POST /{tenant}/tasks/{id}/pushNotificationConfigs",
		"GET /{tenant}/tasks/{id}/pushNotificationConfigs",
		"GET /{tenant}/tasks/{id}/pushNotificationConfigs/{configID}",
		"DELETE /{tenant}/tasks/{id}/pushNotificationConfigs/{configID}",
	} {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, _ *http.Request) {
			writeREST(w, nil, ErrPushNotificationNotSupported())
		})
	}

	return mux
}

// splitVerb separates an AIP colon verb from the resource id it hangs
// off: "arun_1:cancel" is the run and the verb.
func splitVerb(segment string) (id, verb string) {
	at := strings.LastIndex(segment, ":")
	if at < 0 {
		return segment, ""
	}
	return segment[:at], segment[at+1:]
}

func decodeREST(w http.ResponseWriter, r *http.Request, dest any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes))
	if err != nil {
		writeREST(w, nil, ErrParse())
		return false
	}
	if len(body) == 0 {
		return true
	}
	if err := json.Unmarshal(body, dest); err != nil {
		writeREST(w, nil, ErrInvalidParams("the request body could not be decoded"))
		return false
	}
	return true
}

// writeREST renders a result or an error.
//
// A REST client reads the status code first, so an error says the same
// thing twice: the HTTP status the protocol maps it to, and the
// protocol's own numeric code in the body. A client that only understands
// one of the two still understands what happened.
func writeREST(w http.ResponseWriter, result any, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(versionHeader, ProtocolVersion)

	if err != nil {
		perr := asProtocolError(err)
		w.WriteHeader(httpStatusFor(perr))
		// An encode failure means the connection went away mid-write,
		// which there is nothing useful to do about and nobody left to
		// tell.
		if encErr := json.NewEncoder(w).Encode(map[string]any{"error": perr}); encErr != nil {
			return
		}
		return
	}
	if encErr := json.NewEncoder(w).Encode(result); encErr != nil {
		return
	}
}

// httpStatusFor maps a protocol error to the status the specification's
// own error table gives it.
func httpStatusFor(err *Error) int {
	switch err.Code {
	case CodeTaskNotFound:
		return http.StatusNotFound
	case CodeInvalidRequest:
		// The one refusal that is about the caller rather than the
		// request: an unauthenticated peer gets 401 so it knows to
		// present credentials rather than to fix its JSON.
		if err.Message == ErrUnauthenticated().Message {
			return http.StatusUnauthorized
		}
		return http.StatusBadRequest
	case CodeTaskNotCancelable, CodePushNotificationNotSupported, CodeUnsupportedOperation,
		CodeContentTypeNotSupported, CodeExtendedCardNotConfigured, CodeExtensionSupportRequired,
		CodeVersionNotSupported, CodeInvalidParams, CodeParse:
		return http.StatusBadRequest
	case CodeMethodNotFound:
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}
