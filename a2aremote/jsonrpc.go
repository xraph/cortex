package a2aremote

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/skill"
)

// ProtocolVersion is the A2A version this package implements. A request
// that pins a different one is refused rather than answered on a guess.
const ProtocolVersion = "1.0.0"

// versionHeader carries the protocol version a caller expects.
const versionHeader = "A2A-Version"

// maxRequestBytes caps an inbound body.
const maxRequestBytes = 4 << 20

// The JSON-RPC method names. These are the 1.0 spellings; the 0.x ones
// (message/send, tasks/get) are deliberately not served, because
// answering them would tell a client this server speaks a version it
// does not.
const (
	MethodSendMessage          = "SendMessage"
	MethodSendStreamingMessage = "SendStreamingMessage"
	MethodGetTask              = "GetTask"
	MethodListTasks            = "ListTasks"
	MethodCancelTask           = "CancelTask"
	MethodSubscribeToTask      = "SubscribeToTask"
	MethodGetExtendedCard      = "GetExtendedAgentCard"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// JSONRPCHandler serves the JSON-RPC binding.
//
// It decodes, dispatches and encodes. Every decision about what an
// operation MEANS lives in Service, which is what lets the other two
// bindings behave identically without repeating a single rule.
func (s *Service) JSONRPCHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "JSON-RPC requests are POSTed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes))
		if err != nil {
			writeRPC(w, rpcResponse{JSONRPC: "2.0", Error: ErrParse()})
			return
		}

		var req rpcRequest
		if err := json.Unmarshal(body, &req); err != nil {
			writeRPC(w, rpcResponse{JSONRPC: "2.0", Error: ErrParse()})
			return
		}
		// A request with no id is a notification: it is acted on and it
		// gets no response, which JSON-RPC is explicit about.
		notification := len(req.ID) == 0

		resp := s.dispatch(r, req)
		if notification {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeRPC(w, resp)
	})
}

func (s *Service) dispatch(r *http.Request, req rpcRequest) rpcResponse {
	out := rpcResponse{JSONRPC: "2.0", ID: req.ID}

	if req.JSONRPC != "2.0" {
		out.Error = ErrInvalidRequest(`the "jsonrpc" member must be "2.0"`)
		return out
	}
	if v := r.Header.Get(versionHeader); v != "" && v != ProtocolVersion {
		out.Error = ErrVersionNotSupported(v)
		return out
	}

	cred := credentialsOf(r)
	result, err := s.call(r.Context(), cred, req.Method, req.Params)
	if err != nil {
		out.Error = asProtocolError(err)
		return out
	}
	out.Result = result
	return out
}

// call routes one method. The parameter decoding lives here rather than
// in each service method, so the service takes typed requests and never
// sees a wire format.
func (s *Service) call(ctx context.Context, cred Credentials, method string, params json.RawMessage) (any, error) {
	switch method {
	case MethodSendMessage:
		var req SendMessageRequest
		if err := decodeParams(params, &req); err != nil {
			return nil, err
		}
		return s.SendMessage(ctx, cred, req)

	case MethodGetTask:
		var req GetTaskRequest
		if err := decodeParams(params, &req); err != nil {
			return nil, err
		}
		return s.GetTask(ctx, cred, req)

	case MethodListTasks:
		var req ListTasksRequest
		if err := decodeParams(params, &req); err != nil {
			return nil, err
		}
		return s.ListTasks(ctx, cred, req)

	case MethodCancelTask:
		var req CancelTaskRequest
		if err := decodeParams(params, &req); err != nil {
			return nil, err
		}
		return s.CancelTask(ctx, cred, req)

	case MethodSendStreamingMessage, MethodSubscribeToTask:
		// Declared unsupported in the card too, so a client that read the
		// card never gets here.
		return nil, ErrUnsupportedOperation(method)

	case MethodGetExtendedCard:
		return nil, ErrExtendedCardNotConfigured()

	default:
		return nil, ErrMethodNotFound(method)
	}
}

func decodeParams(params json.RawMessage, dest any) error {
	if len(params) == 0 {
		return nil
	}
	if err := json.Unmarshal(params, dest); err != nil {
		return ErrInvalidParams("params could not be decoded: " + err.Error())
	}
	return nil
}

// asProtocolError keeps a service error's code and turns anything else
// into an internal error without leaking its text. A peer must be able
// to tell "no such task" from "the server broke", and must not learn the
// shape of our internals from an error string.
func asProtocolError(err error) *Error {
	var perr *Error
	if errors.As(err, &perr) {
		return perr
	}
	return ErrInternal("the request could not be completed")
}

func writeRPC(w http.ResponseWriter, resp rpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(versionHeader, ProtocolVersion)
	// The status stays 200 even for an error: a JSON-RPC error is a
	// well-formed response, and a client reading HTTP status instead of
	// the error member would see a transport failure that did not happen.
	//
	// An encode failure means the connection went away mid-write, which
	// there is nothing useful to do about and nobody left to tell.
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		return
	}
}

// credentialsOf lifts what a resolver can authenticate on out of the
// request.
func credentialsOf(r *http.Request) Credentials {
	return Credentials{
		Headers:    r.Header,
		RemoteAddr: r.RemoteAddr,
		TLS:        r.TLS,
	}
}

// CardHandler serves agent cards.
//
// Only exposed agents are served, and an unexposed one is a 404 rather
// than a 403: a card is public, so the fact that an agent exists here at
// all is disclosure.
func (s *Service) CardHandler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /agents/{name}"+WellKnownCardPath, func(w http.ResponseWriter, r *http.Request) {
		s.writeCard(w, r, r.PathValue("name"))
	})
	if s.opts.DefaultAgent != "" {
		mux.HandleFunc("GET "+WellKnownCardPath, func(w http.ResponseWriter, r *http.Request) {
			s.writeCard(w, r, s.opts.DefaultAgent)
		})
	}
	return mux
}

func (s *Service) writeCard(w http.ResponseWriter, r *http.Request, name string) {
	if !s.exposed(name) {
		http.NotFound(w, r)
		return
	}
	// A card is read without credentials, so there is no caller scope to
	// borrow. The host names the scope its exposed agents live in, and
	// only agents it exposed are ever read under it.
	ctx := r.Context()
	if !s.opts.Scope.IsZero() {
		ctx = cortex.WithScope(ctx, s.opts.Scope)
	}

	a, err := s.gw.GetAgentByName(ctx, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	opts := s.opts.Card
	card := BuildCard(a, s.skillsOf(ctx, a), opts)

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(card); err != nil {
		return
	}
}

func (s *Service) exposed(name string) bool {
	for _, n := range s.opts.Exposed {
		if strings.EqualFold(n, name) {
			return true
		}
	}
	return false
}

// skillsOf resolves an agent's inline skills for its card, skipping any
// the store cannot produce: a card missing one skill is better than no
// card at all.
func (s *Service) skillsOf(ctx context.Context, a *agent.Config) []*skill.Skill {
	out := make([]*skill.Skill, 0, len(a.InlineSkills))
	for _, name := range a.InlineSkills {
		sk, err := s.gw.GetSkillByName(ctx, name)
		if err != nil {
			continue
		}
		out = append(out, sk)
	}
	return out
}
