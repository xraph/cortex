package grpcbind_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/xraph/cortex/a2aremote"
	"github.com/xraph/cortex/a2aremote/grpcbind"
	"github.com/xraph/cortex/a2aremote/grpcbind/a2apb"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/event"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

type replayGateway struct {
	*gateway
	owner id.AgentID
	runID id.AgentRunID
}

func (g *replayGateway) GetAgentByName(context.Context, string) (*agent.Config, error) {
	return &agent.Config{ID: g.owner, Name: "worker"}, nil
}
func (g *replayGateway) PublicEventReplayAvailable() bool { return true }
func (g *replayGateway) ReadPublicEvents(ctx context.Context, r event.ReadRequest) (event.Page, error) {
	p, ok := a2aremote.PeerFromContext(ctx)
	if !ok || p.Node != "peer.example" {
		return event.Page{}, event.ErrAccessDenied
	}
	if r.Limit == 1 {
		return event.Page{}, errors.Join(event.ErrAccessDenied, errors.New("SECRET cause"))
	}
	const seq = 9007199254740993
	page := event.Page{Watermark: event.Watermark{SchemaVersion: 1, RunID: g.runID, FirstSequence: seq, LastSequence: seq, RunRevision: seq}, Next: event.Cursor{SchemaVersion: 1, RunID: g.runID, Sequence: seq}}
	if r.After.Sequence == seq-1 {
		page.Events = []event.Event{{SchemaVersion: 1, Input: event.Input{ID: id.New(event.PrefixEvent), Type: event.ArtifactCreated, ResultID: id.New(verification.PrefixResult)}, RunID: g.runID, Sequence: seq, RunRevision: seq, ObservedAt: time.Now().UTC()}}
	}
	return page, nil
}

func TestGRPCReplayPreservesPrecisionAndActivation(t *testing.T) {
	gw := &replayGateway{gateway: newGateway(), owner: id.NewAgentID(), runID: id.NewAgentRunID()}
	gw.runs[gw.runID.String()] = &run.Run{ID: gw.runID, AgentID: gw.owner, State: run.StateCompleted, Revision: 9007199254740993, Output: "SECRET candidate"}
	client := dialWith(t, gw, okResolver(), a2aremote.Options{PublicEventReplay: true})
	ctx := metadata.AppendToOutgoingContext(authed(t.Context()), "a2a-extensions", a2aremote.ReplayExtensionURI, "cortex-replay-cursor", "1."+gw.runID.String()+".9007199254740992")
	var header metadata.MD
	task, err := client.GetTask(ctx, &a2apb.GetTaskRequest{Tenant: "worker", Id: gw.runID.String()}, grpc.Header(&header))
	if err != nil {
		t.Fatal(err)
	}
	if got := header.Get("a2a-extensions"); len(got) != 1 || got[0] != a2aremote.ReplayExtensionURI {
		t.Fatalf("missing activation: %v", header)
	}
	if len(task.Artifacts) != 0 || task.Status.Message != nil {
		t.Fatal("replay leaked content")
	}
	page := task.Metadata.Fields[a2aremote.ReplayMetadataKey].GetStructValue()
	observation := page.Fields["events"].GetListValue().Values[0].GetStructValue()
	if observation.Fields["sequence"].GetStringValue() != "9007199254740993" || observation.Fields["run_revision"].GetStringValue() != "9007199254740993" {
		t.Fatalf("precision lost: %v", observation)
	}
	if observation.Fields["type"].GetStringValue() != "artifact_created" || !strings.HasPrefix(observation.Fields["result_id"].GetStringValue(), "vresult_") {
		t.Fatalf("artifact metadata lost: %v", observation)
	}
	next := page.Fields["next_cursor"].GetStringValue()
	ctx = metadata.AppendToOutgoingContext(authed(t.Context()), "a2a-extensions", a2aremote.ReplayExtensionURI, "cortex-replay-cursor", next)
	resumed, err := client.GetTask(ctx, &a2apb.GetTaskRequest{Tenant: "worker", Id: gw.runID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed.Metadata.Fields[a2aremote.ReplayMetadataKey].GetStructValue().Fields["events"].GetListValue().Values) != 0 {
		t.Fatal("duplicate resumed event")
	}
	ctx = metadata.AppendToOutgoingContext(ctx, "cortex-replay-limit", "1")
	_, err = client.GetTask(ctx, &a2apb.GetTaskRequest{Tenant: "worker", Id: gw.runID.String()})
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("unsafe failure: %v", err)
	}
	details := status.Convert(err).Details()
	if len(details) != 1 {
		t.Fatalf("missing safe retry details: %v", details)
	}
	detail, ok := details[0].(*structpb.Struct)
	if !ok || !strings.Contains(detail.String(), "replay_denied") {
		t.Fatalf("bad details: %v", details)
	}
}

type replaySubscriptionGateway struct {
	*replayGateway
	reads int
}

func (g *replaySubscriptionGateway) ReadPublicEvents(ctx context.Context, r event.ReadRequest) (event.Page, error) {
	g.reads++
	if g.reads == 2 {
		g.runs[g.runID.String()].State = run.StateCompleted
	}
	return g.replayGateway.ReadPublicEvents(ctx, r)
}

func TestGRPCReplaySubscriptionAndDeadline(t *testing.T) {
	for _, mode := range []string{"bounded", "missing", "long"} {
		t.Run(mode, func(t *testing.T) {
			base := &replayGateway{gateway: newGateway(), owner: id.NewAgentID(), runID: id.NewAgentRunID()}
			base.runs[base.runID.String()] = &run.Run{ID: base.runID, AgentID: base.owner, State: run.StateRunning, Revision: 9007199254740993, Output: "SECRET candidate"}
			gw := &replaySubscriptionGateway{replayGateway: base}
			client := dialWith(t, gw, okResolver(), a2aremote.Options{PublicEventReplay: true, Streaming: true})
			ctx := metadata.AppendToOutgoingContext(authed(t.Context()), "a2a-extensions", a2aremote.ReplayExtensionURI, "cortex-replay-cursor", "1."+base.runID.String()+".9007199254740992")
			if mode != "missing" {
				duration := 30 * time.Second
				if mode == "long" {
					duration = 6 * time.Minute
				}
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, duration)
				defer cancel()
			}
			stream, err := client.SubscribeToTask(ctx, &a2apb.SubscribeToTaskRequest{Tenant: "worker", Id: base.runID.String()})
			if err != nil {
				t.Fatal(err)
			}
			first, err := stream.Recv()
			if mode != "bounded" {
				if status.Code(err) != codes.InvalidArgument || first != nil {
					t.Fatalf("unbounded subscription accepted: %v %v", first, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			headers, err := stream.Header()
			if err != nil || len(headers.Get("a2a-extensions")) != 1 {
				t.Fatalf("missing negotiation: %v %v", headers, err)
			}
			if first.GetTask() == nil || len(first.GetTask().Artifacts) != 0 || strings.Contains(first.String(), "SECRET") {
				t.Fatalf("bad snapshot: %v", first)
			}
			next, err := stream.Recv()
			if err != nil || next.GetStatusUpdate() == nil || next.GetStatusUpdate().Status.State != a2apb.TaskState_TASK_STATE_COMPLETED || strings.Contains(next.String(), "SECRET") {
				t.Fatalf("bad completion: %v %v", next, err)
			}
			if _, err = stream.Recv(); !errors.Is(err, io.EOF) {
				t.Fatalf("stream did not close: %v", err)
			}
		})
	}
}

type stalledReplayStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *stalledReplayStream) Context() context.Context         { return s.ctx }
func (s *stalledReplayStream) SendHeader(metadata.MD) error     { return nil }
func (s *stalledReplayStream) Send(*a2apb.StreamResponse) error { <-s.ctx.Done(); return s.ctx.Err() }

func TestGRPCReplayStalledSendHonorsClientDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gw := &replayGateway{gateway: newGateway(), owner: id.NewAgentID(), runID: id.NewAgentRunID()}
		gw.runs[gw.runID.String()] = &run.Run{ID: gw.runID, AgentID: gw.owner, State: run.StateRunning, Revision: 9007199254740993}
		svc := a2aremote.NewService(gw, okResolver(), a2aremote.Options{PublicEventReplay: true, Streaming: true})
		ctx := metadata.NewIncomingContext(t.Context(), metadata.Pairs("authorization", "Bearer ok", "a2a-extensions", a2aremote.ReplayExtensionURI, "cortex-replay-cursor", "1."+gw.runID.String()+".9007199254740992"))
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		start := time.Now()
		err := grpcbind.NewServer(svc).SubscribeToTask(&a2apb.SubscribeToTaskRequest{Tenant: "worker", Id: gw.runID.String()}, &stalledReplayStream{ctx: ctx})
		if err == nil || time.Since(start) != 5*time.Second {
			t.Fatalf("unbounded send: %v elapsed=%v", err, time.Since(start))
		}
	})
}
