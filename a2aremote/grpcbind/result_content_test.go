package grpcbind_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/xraph/cortex/a2aremote"
	"github.com/xraph/cortex/a2aremote/grpcbind/a2apb"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/safety"
	"github.com/xraph/cortex/verification"
)

type resultGateway struct{ *replayGateway }

func (g *resultGateway) ResultContentAvailable() bool { return true }
func (g *resultGateway) ReadResultContent(ctx context.Context, req verification.ResultContentRequest) (verification.PublishedResult, error) {
	if peer, ok := a2aremote.PeerFromContext(ctx); !ok || peer.Node != "peer.example" {
		return verification.PublishedResult{}, verification.ErrResultAccessDenied
	}
	return verification.PublishedResult{SchemaVersion: 1, RunID: req.RunID, ResultID: req.ResultID, RunRevision: 9007199254740993, Content: "screened", PublicationStatus: safety.StatusRedacted, ContentChanged: true, VerificationStatus: verification.StatusPassed, VerifiedAt: time.Now(), CheckedAt: time.Now()}, nil
}

func TestGRPCResultContent(t *testing.T) {
	gw := &resultGateway{replayGateway: &replayGateway{gateway: newGateway(), owner: id.NewAgentID(), runID: id.NewAgentRunID()}}
	gw.runs[gw.runID.String()] = &run.Run{ID: gw.runID, AgentID: gw.owner, State: run.StateCompleted, Revision: 9007199254740993, Output: "SECRET candidate"}
	client := dialWith(t, gw, okResolver(), a2aremote.Options{ResultContent: true})
	ctx := metadata.AppendToOutgoingContext(authed(t.Context()), "a2a-extensions", a2aremote.ResultContentExtensionURI, "cortex-result-id", id.New(verification.PrefixResult).String())
	req := &a2apb.GetTaskRequest{Tenant: "worker", Id: gw.runID.String()}
	if _, err := client.GetTask(ctx, req); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unbounded content call accepted: %v", err)
	}
	long, cancelLong := context.WithTimeout(ctx, time.Minute)
	defer cancelLong()
	if _, err := client.GetTask(long, req); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("excessive content deadline accepted: %v", err)
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var header metadata.MD
	task, err := client.GetTask(bounded, req, grpc.Header(&header))
	if err != nil {
		t.Fatal(err)
	}
	if got := header.Get("a2a-extensions"); len(got) != 1 || got[0] != a2aremote.ResultContentExtensionURI {
		t.Fatalf("missing activation: %v", header)
	}
	if got := header.Get("cache-control"); len(got) != 1 || got[0] != "no-store" {
		t.Fatalf("missing private cache metadata: %v", header)
	}
	result := task.Metadata.Fields[a2aremote.ResultContentMetadataKey].GetStructValue()
	if result == nil || result.Fields["run_revision"].GetStringValue() != "9007199254740993" || result.Fields["content"].GetStringValue() != "screened" || strings.Contains(task.String(), "SECRET") || len(task.Artifacts) != 0 {
		t.Fatalf("unsafe gRPC result: %v", task)
	}
	req.Id = id.NewAgentRunID().String()
	if _, err = client.GetTask(bounded, req); status.Code(err) != codes.NotFound || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("missing result error: %v", err)
	}
}
