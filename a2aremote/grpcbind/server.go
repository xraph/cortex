// Package grpcbind serves the A2A gRPC binding over the same service the
// other two bindings use.
//
// It is a module of its own rather than a package inside a2aremote, and
// the reason is dependency weight: gRPC and protobuf are a large graph,
// and a host serving JSON-RPC or HTTP+JSON has no business inheriting
// them. Importing this module is the opt-in.
//
// The generated types in a2apb come from the normative a2a.proto,
// vendored verbatim. Everything here is translation between those types
// and the ones a2aremote already defines; not one decision about what an
// operation means lives on this side of the boundary.
package grpcbind

import (
	"context"
	"errors"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/xraph/cortex/a2aremote"
	"github.com/xraph/cortex/a2aremote/grpcbind/a2apb"
)

// Server adapts a2aremote.Service to the generated gRPC service.
type Server struct {
	a2apb.UnimplementedA2AServiceServer
	svc *a2aremote.Service
}

// NewServer wraps a service for gRPC.
func NewServer(svc *a2aremote.Service) *Server { return &Server{svc: svc} }

// Register adds the service to a gRPC server.
func Register(s grpc.ServiceRegistrar, svc *a2aremote.Service) {
	a2apb.RegisterA2AServiceServer(s, NewServer(svc))
}

// SendMessage carries an inbound message to the agent named by tenant.
func (s *Server) SendMessage(ctx context.Context, req *a2apb.SendMessageRequest) (*a2apb.SendMessageResponse, error) {
	result, err := s.svc.SendMessage(ctx, credentialsOf(ctx), a2aremote.SendMessageRequest{
		Tenant:  req.GetTenant(),
		Message: messageFromProto(req.GetMessage()),
	})
	if err != nil {
		return nil, statusOf(err)
	}

	out := &a2apb.SendMessageResponse{}
	switch {
	case result.Task != nil:
		out.Payload = &a2apb.SendMessageResponse_Task{Task: taskToProto(result.Task)}
	case result.Message != nil:
		out.Payload = &a2apb.SendMessageResponse_Message{Message: messageToProto(result.Message)}
	}
	return out, nil
}

// GetTask projects a run as a task.
func (s *Server) GetTask(ctx context.Context, req *a2apb.GetTaskRequest) (*a2apb.Task, error) {
	task, err := s.svc.GetTask(ctx, credentialsOf(ctx), a2aremote.GetTaskRequest{
		Tenant: req.GetTenant(), ID: req.GetId(),
	})
	if err != nil {
		return nil, statusOf(err)
	}
	return taskToProto(task), nil
}

// ListTasks projects this scope's runs as tasks.
func (s *Server) ListTasks(ctx context.Context, req *a2apb.ListTasksRequest) (*a2apb.ListTasksResponse, error) {
	result, err := s.svc.ListTasks(ctx, credentialsOf(ctx), a2aremote.ListTasksRequest{
		Tenant: req.GetTenant(), PageSize: int(req.GetPageSize()),
	})
	if err != nil {
		return nil, statusOf(err)
	}
	out := &a2apb.ListTasksResponse{Tasks: make([]*a2apb.Task, 0, len(result.Tasks))}
	for i := range result.Tasks {
		out.Tasks = append(out.Tasks, taskToProto(&result.Tasks[i]))
	}
	return out, nil
}

// CancelTask stops a running task.
func (s *Server) CancelTask(ctx context.Context, req *a2apb.CancelTaskRequest) (*a2apb.Task, error) {
	task, err := s.svc.CancelTask(ctx, credentialsOf(ctx), a2aremote.CancelTaskRequest{
		Tenant: req.GetTenant(), ID: req.GetId(),
	})
	if err != nil {
		return nil, statusOf(err)
	}
	return taskToProto(task), nil
}

// credentialsOf lifts gRPC metadata into the transport-neutral shape a
// resolver takes, so one resolver serves all three bindings.
func credentialsOf(ctx context.Context) a2aremote.Credentials {
	cred := a2aremote.Credentials{}
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		cred.Headers = md
	}
	if p, ok := peer.FromContext(ctx); ok {
		if p.Addr != nil {
			cred.RemoteAddr = p.Addr.String()
		}
		if tlsInfo, ok := p.AuthInfo.(interface{ GetSecurityValue() any }); ok {
			_ = tlsInfo // TLS details arrive through AuthInfo; nothing needs them yet.
		}
	}
	return cred
}

// statusOf maps a protocol error onto a gRPC status, using the code
// mapping the specification's own error table gives.
func statusOf(err error) error {
	var perr *a2aremote.Error
	if !errors.As(err, &perr) {
		return status.Error(codes.Internal, "the request could not be completed")
	}
	return status.Error(grpcCodeFor(perr), perr.Message)
}

func grpcCodeFor(err *a2aremote.Error) codes.Code {
	switch err.Code {
	case a2aremote.CodeTaskNotFound, a2aremote.CodeMethodNotFound:
		return codes.NotFound
	case a2aremote.CodeTaskNotCancelable, a2aremote.CodePushNotificationNotSupported,
		a2aremote.CodeUnsupportedOperation, a2aremote.CodeExtendedCardNotConfigured,
		a2aremote.CodeExtensionSupportRequired, a2aremote.CodeVersionNotSupported:
		return codes.FailedPrecondition
	case a2aremote.CodeContentTypeNotSupported, a2aremote.CodeInvalidParams, a2aremote.CodeParse:
		return codes.InvalidArgument
	case a2aremote.CodeInvalidRequest:
		// The refusal that is about the caller rather than the request.
		if err.Message == a2aremote.ErrUnauthenticated().Message {
			return codes.Unauthenticated
		}
		return codes.InvalidArgument
	default:
		return codes.Internal
	}
}

func messageFromProto(m *a2apb.Message) a2aremote.Message {
	if m == nil {
		return a2aremote.Message{}
	}
	out := a2aremote.Message{
		MessageID:        m.GetMessageId(),
		ContextID:        m.GetContextId(),
		TaskID:           m.GetTaskId(),
		Role:             roleFromProto(m.GetRole()),
		Extensions:       m.GetExtensions(),
		ReferenceTaskIDs: m.GetReferenceTaskIds(),
	}
	if md := m.GetMetadata(); md != nil {
		out.Metadata = md.AsMap()
	}
	for _, p := range m.GetParts() {
		out.Parts = append(out.Parts, partFromProto(p))
	}
	return out
}

func partFromProto(p *a2apb.Part) a2aremote.Part {
	switch content := p.GetContent().(type) {
	case *a2apb.Part_Text:
		return a2aremote.Part{Text: content.Text}
	case *a2apb.Part_Raw:
		return a2aremote.Part{File: &a2aremote.FilePart{Raw: content.Raw}}
	case *a2apb.Part_Url:
		return a2aremote.Part{File: &a2aremote.FilePart{URL: content.Url}}
	case *a2apb.Part_Data:
		// A data part is refused by the mapping layer anyway, so it is
		// carried across as an empty one: the refusal names the shape,
		// not its contents.
		return a2aremote.Part{Data: &a2aremote.DataPart{}}
	default:
		return a2aremote.Part{}
	}
}

func messageToProto(m *a2aremote.Message) *a2apb.Message {
	if m == nil {
		return nil
	}
	out := &a2apb.Message{
		MessageId:        m.MessageID,
		ContextId:        m.ContextID,
		TaskId:           m.TaskID,
		Role:             roleToProto(m.Role),
		Extensions:       m.Extensions,
		ReferenceTaskIds: m.ReferenceTaskIDs,
	}
	for _, p := range m.Parts {
		out.Parts = append(out.Parts, &a2apb.Part{Content: &a2apb.Part_Text{Text: p.Text}})
	}
	return out
}

func taskToProto(t *a2aremote.Task) *a2apb.Task {
	if t == nil {
		return nil
	}
	out := &a2apb.Task{
		Id:        t.ID,
		ContextId: t.ContextID,
		Status: &a2apb.TaskStatus{
			State:     taskStateToProto(t.Status.State),
			Message:   messageToProto(t.Status.Message),
			Timestamp: timestamppb.Now(),
		},
	}
	for i := range t.Artifacts {
		a := &t.Artifacts[i]
		pa := &a2apb.Artifact{ArtifactId: a.ArtifactID, Name: a.Name, Description: a.Description}
		for _, p := range a.Parts {
			pa.Parts = append(pa.Parts, &a2apb.Part{Content: &a2apb.Part_Text{Text: p.Text}})
		}
		out.Artifacts = append(out.Artifacts, pa)
	}
	return out
}

func taskStateToProto(s a2aremote.TaskState) a2apb.TaskState {
	switch s {
	case a2aremote.TaskStateSubmitted:
		return a2apb.TaskState_TASK_STATE_SUBMITTED
	case a2aremote.TaskStateWorking:
		return a2apb.TaskState_TASK_STATE_WORKING
	case a2aremote.TaskStateCompleted:
		return a2apb.TaskState_TASK_STATE_COMPLETED
	case a2aremote.TaskStateFailed:
		return a2apb.TaskState_TASK_STATE_FAILED
	case a2aremote.TaskStateCanceled:
		return a2apb.TaskState_TASK_STATE_CANCELED
	case a2aremote.TaskStateRejected:
		return a2apb.TaskState_TASK_STATE_REJECTED
	case a2aremote.TaskStateInputRequired:
		return a2apb.TaskState_TASK_STATE_INPUT_REQUIRED
	case a2aremote.TaskStateAuthRequired:
		return a2apb.TaskState_TASK_STATE_AUTH_REQUIRED
	default:
		return a2apb.TaskState_TASK_STATE_UNSPECIFIED
	}
}

func roleFromProto(r a2apb.Role) a2aremote.Role {
	if r == a2apb.Role_ROLE_AGENT {
		return a2aremote.RoleAgent
	}
	return a2aremote.RoleUser
}

func roleToProto(r a2aremote.Role) a2apb.Role {
	if r == a2aremote.RoleAgent {
		return a2apb.Role_ROLE_AGENT
	}
	return a2apb.Role_ROLE_USER
}

// SendStreamingMessage sends a message and streams the task's progress.
func (s *Server) SendStreamingMessage(req *a2apb.SendMessageRequest, stream a2apb.A2AService_SendStreamingMessageServer) error {
	return s.svc.StreamMessage(stream.Context(), credentialsOf(stream.Context()), a2aremote.SendMessageRequest{
		Tenant:  req.GetTenant(),
		Message: messageFromProto(req.GetMessage()),
	}, emitTo(stream))
}

// SubscribeToTask streams the progress of a task already running.
func (s *Server) SubscribeToTask(req *a2apb.SubscribeToTaskRequest, stream a2apb.A2AService_SubscribeToTaskServer) error {
	return s.svc.SubscribeTask(stream.Context(), credentialsOf(stream.Context()),
		req.GetTenant(), req.GetId(), emitTo(stream))
}

// streamSender is the half of a generated server stream this package
// uses. Naming it lets both streaming methods share one adapter rather
// than repeating the translation twice.
type streamSender interface {
	Send(*a2apb.StreamResponse) error
}

// emitTo adapts a generated stream to the service's emitter.
//
// A send failure ends the stream by returning, which is how a gRPC
// server learns its client hung up.
func emitTo(stream streamSender) a2aremote.Emit {
	return func(ev a2aremote.StreamEvent) error {
		return stream.Send(streamResponseOf(ev))
	}
}

func streamResponseOf(ev a2aremote.StreamEvent) *a2apb.StreamResponse {
	switch {
	case ev.Task != nil:
		return &a2apb.StreamResponse{Payload: &a2apb.StreamResponse_Task{Task: taskToProto(ev.Task)}}
	case ev.Message != nil:
		return &a2apb.StreamResponse{Payload: &a2apb.StreamResponse_Message{Message: messageToProto(ev.Message)}}
	case ev.StatusUpdate != nil:
		return &a2apb.StreamResponse{Payload: &a2apb.StreamResponse_StatusUpdate{
			StatusUpdate: &a2apb.TaskStatusUpdateEvent{
				TaskId:    ev.StatusUpdate.TaskID,
				ContextId: ev.StatusUpdate.ContextID,
				Status: &a2apb.TaskStatus{
					State:     taskStateToProto(ev.StatusUpdate.Status.State),
					Message:   messageToProto(ev.StatusUpdate.Status.Message),
					Timestamp: timestamppb.Now(),
				},
			},
		}}
	case ev.ArtifactUpdate != nil:
		artifact := &a2apb.Artifact{
			ArtifactId: ev.ArtifactUpdate.Artifact.ArtifactID,
			Name:       ev.ArtifactUpdate.Artifact.Name,
		}
		for _, p := range ev.ArtifactUpdate.Artifact.Parts {
			artifact.Parts = append(artifact.Parts, &a2apb.Part{Content: &a2apb.Part_Text{Text: p.Text}})
		}
		return &a2apb.StreamResponse{Payload: &a2apb.StreamResponse_ArtifactUpdate{
			ArtifactUpdate: &a2apb.TaskArtifactUpdateEvent{
				TaskId:    ev.ArtifactUpdate.TaskID,
				ContextId: ev.ArtifactUpdate.ContextID,
				Artifact:  artifact,
			},
		}}
	default:
		return &a2apb.StreamResponse{}
	}
}
