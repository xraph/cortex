//go:build linux && sandbox_peer_kernel

package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/sandbox"
)

func TestControlKernelWorkerBootstrapUsesPrivateDaemon(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("worker bootstrap transport fixture requires root")
	}
	name, directory, credential, boundary := workerBootstrapFixture(t, declareCapture)
	listener, socket := controlListener(t)
	loaded, err := readWorkerConfig(t.Context(), name, 0)
	if err != nil {
		t.Fatal(err)
	}
	loaded.Settings.ControlSocket = socket
	writeWorkerConfig(t, name, loaded.Settings)
	loaded, err = readWorkerConfig(t.Context(), name, 0)
	if err != nil {
		t.Fatal(err)
	}
	credential.ProfileDigest = loaded.ProfileDigest
	writeBootstrapCredential(t, directory, credential)
	var authorizations atomic.Int32
	server := &WorkerControl{limits: ControlLimits{Connections: 2, Reports: 1, Timeout: time.Second}, authorize: func(_ context.Context, _ *net.UnixConn, claim WorkerClaim) error {
		if claim.JobID != credential.Record.Receipt.Job.ID || claim.Token != credential.Token || claim.ProfileDigest != loaded.ProfileDigest {
			return broker.ErrDenied
		}
		if authorizations.Add(1) == 1 {
			return errWorkerActivating
		}
		return nil
	}, beginReport: func(_ context.Context, _ *net.UnixConn, claim WorkerClaim) (reportSession, error) {
		captureSession := &fixtureCaptureSession{fixtureReportSession: fixtureReportSession{claim: claim}}
		return &struct {
			*journalEvidenceSession
			imageSession
		}{
			&journalEvidenceSession{fixtureEvidenceSession: fixtureEvidenceSession{fixtureReportSession: fixtureReportSession{claim: claim}, data: []byte("candidate")}, capture: captureSession.Capture},
			&fixtureImageSession{data: []byte("image data")},
		}, nil
	}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.serve(ctx, listener) }()
	// Keep the service-membership seam explicit: this checks real daemon socket
	// authentication and startup/capture/report exchanges, not a live systemd worker.
	boundary.authorize, boundary.report, boundary.capture = AuthorizeAt, ReportAt, CaptureAt
	boundary.readEvidence, boundary.putArtifact = ReadEvidenceAt, PutArtifactAt
	boundary.readImage = ReadImageAt
	worker, err := bootstrapWorker(t.Context(), name, directory, credential.Record.Receipt.Job.ID, boundary)
	if err != nil || authorizations.Load() != 2 {
		t.Fatal("authenticated activation handshake failed", err)
	}
	if err = worker.BeginExecution(t.Context()); err != nil {
		t.Fatal(err)
	}
	job := credential.Record.Receipt.Job
	imageReader, imageErr := worker.ImageSource().Open(cortex.WithScope(t.Context(), job.Scope), imagePin([]byte("image data")))
	if imageErr != nil {
		t.Fatal(imageErr)
	}
	imageBytes, imageErr := io.ReadAll(imageReader)
	imageCloseErr := imageReader.Close()
	if imageErr != nil || imageCloseErr != nil || string(imageBytes) != "image data" {
		t.Fatal("private image source failed", imageErr, imageCloseErr)
	}
	ref := captureRefForJob(t, job)
	if err = worker.CaptureJournal().Record(t.Context(), job, ref); err != nil {
		t.Fatal("private intent failed", err)
	}
	evidenceContext := cortex.WithScope(t.Context(), job.Scope)
	if _, err = worker.ArtifactRepository().Put(evidenceContext, evidence.Artifact{Ref: ref, Data: []byte("candidate")}); err != nil {
		t.Fatal("private upload failed", err)
	}
	reader, readErr := worker.ArtifactRepository().Open(evidenceContext, ref)
	if readErr != nil {
		t.Fatal(readErr)
	}
	data, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || string(data) != "candidate" {
		t.Fatal("private readback failed", readErr, closeErr)
	}
	if err = worker.CaptureJournal().Confirm(t.Context(), job, ref); err != nil {
		t.Fatal("private confirmation failed", err)
	}
	result := sandbox.Result{JobDigest: credential.Record.Receipt.Job.Digest, State: sandbox.StateExited}
	if _, err = worker.Report(t.Context(), result); err != nil {
		t.Fatal("bootstrapped worker report failed", err)
	}
	if err = os.Chmod(socket, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err = CaptureAt(t.Context(), socket, worker.claim, ref, false); err == nil {
		t.Fatal("capture sent through unsafe endpoint")
	}
	if err = os.Chmod(socket, 0o600); err != nil {
		t.Fatal(err)
	}
	if ambient, ambientErr := BootstrapWorker(t.Context(), name, directory, credential.Record.Receipt.Job.ID); ambientErr == nil || ambient != nil {
		t.Fatal("ambient root process bootstrapped outside its worker service")
	}
	cancel()
	_ = awaitControllerStop(t, done)
}

func TestControlKernelAuthenticatesPrivateDaemonSocket(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("worker control kernel tests require root")
	}
	path, cancel, done := startControl(t, func(context.Context, *net.UnixConn, WorkerClaim) error { return nil }, ControlLimits{Connections: 1, Timeout: time.Second})
	if err := AuthorizeAt(t.Context(), path, controlClaim()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := AuthorizeAt(t.Context(), path, controlClaim()); err == nil {
		t.Fatal("public endpoint accepted")
	}
	cancel()
	_ = awaitControllerStop(t, done)
}

func TestControlKernelPublicServerRejectsAmbientRootAndJoinsController(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("worker control kernel tests require root")
	}
	controller, _, _ := controllerFixture(t)
	cancelController, controllerDone := runController(t, controller)
	awaitControllerReady(t, controller)
	server, err := NewWorkerControl(controller, ControlLimits{Connections: 2, Reports: 1, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	listener, path := controlListener(t)
	served := make(chan error, 1)
	go func() { served <- server.Serve(t.Context(), listener) }()
	if err = AuthorizeAt(t.Context(), path, controlClaim()); !errors.Is(err, broker.ErrDenied) {
		t.Fatalf("ambient root result: %v", err)
	}
	if _, err = ReportAt(t.Context(), path, controlClaim(), testWireResult()); !errors.Is(err, broker.ErrDenied) {
		t.Fatalf("ambient root report: %v", err)
	}
	if _, err = CaptureAt(t.Context(), path, controlClaim(), wireCaptureRef(t), false); !errors.Is(err, broker.ErrDenied) {
		t.Fatal("ambient root captured", err)
	}
	cancelController()
	if err = awaitControllerStop(t, controllerDone); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err = awaitControllerStop(t, served); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err = os.Lstat(path); err != nil {
		t.Fatal("server removed host-owned socket path")
	}
}

func TestControlKernelReportClientAuthenticatesPrivateDaemon(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("worker control kernel tests require root")
	}
	claim := controlClaim()
	path, cancel, done := startReportControl(t, func(context.Context, *net.UnixConn, WorkerClaim) (reportSession, error) {
		return &fixtureReportSession{claim: claim}, nil
	}, ControlLimits{Connections: 2, Reports: 1, Timeout: time.Second})
	if receipt, err := ReportAt(t.Context(), path, claim, testWireResult()); err != nil || receipt.JobID != claim.JobID {
		t.Fatal("private report client failed", err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := ReportAt(t.Context(), path, claim, testWireResult()); err == nil {
		t.Fatal("report sent through unsafe endpoint")
	}
	cancel()
	_ = awaitControllerStop(t, done)
}

func TestControlKernelEvidenceClientsAuthenticatePrivateDaemon(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("worker evidence transport fixture requires root")
	}
	claim := controlClaim()
	a := evidence.Artifact{Ref: wireCaptureRef(t), Data: []byte("candidate")}
	session := &fixtureEvidenceSession{fixtureReportSession: fixtureReportSession{claim: claim}}
	path, cancel, done := startReportControl(t, func(context.Context, *net.UnixConn, WorkerClaim) (reportSession, error) { return session, nil }, ControlLimits{Connections: 2, Reports: 1, Timeout: time.Second})
	defer func() { cancel(); _ = awaitControllerStop(t, done) }()
	if _, err := PutArtifactAt(t.Context(), path, claim, a); err != nil {
		t.Fatal(err)
	}
	for _, artifact := range []bool{false, true} {
		var content bytes.Buffer
		if err := ReadEvidenceAt(t.Context(), path, claim, a.Ref, artifact, &content); err != nil || !bytes.Equal(content.Bytes(), a.Data) {
			t.Fatal("private evidence read", err)
		}
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := PutArtifactAt(t.Context(), path, claim, a); err == nil {
		t.Fatal("upload reached unsafe endpoint")
	}
	if err := ReadEvidenceAt(t.Context(), path, claim, a.Ref, false, io.Discard); err == nil {
		t.Fatal("download reached unsafe endpoint")
	}
	if session.closes.Load() != 3 {
		t.Fatal("unsafe endpoint acquired service peer")
	}
}

func TestControlKernelEvidenceServiceBindingAndAmbientRootDenial(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("worker evidence transport fixture requires root")
	}
	access, c, claim, _, input, output, repo := workerEvidenceFixture(t)
	limits := ControlLimits{Connections: 2, Reports: 1, Timeout: time.Second}
	for _, providers := range [][]WorkerContent{{{}}, {{Evidence: access}, {Evidence: access}}, {{Evidence: &WorkerEvidence{controller: &Controller{}}}}} {
		if server, err := NewWorkerControl(c, limits, providers...); err == nil || server != nil {
			t.Fatal("invalid evidence binding accepted")
		}
	}
	server, err := NewWorkerControl(c, limits, WorkerContent{Evidence: access})
	if err != nil {
		t.Fatal(err)
	}
	listener, path := controlListener(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	if _, err = PutArtifactAt(t.Context(), path, claim, output); err == nil {
		t.Fatal("ambient root uploaded evidence")
	}
	if err = ReadEvidenceAt(t.Context(), path, claim, input.Ref, false, io.Discard); err == nil {
		t.Fatal("ambient root read evidence")
	}
	cancel()
	_ = awaitControllerStop(t, done)
	if repo.puts != 0 || repo.opens != 0 {
		t.Fatal("ambient root reached provider")
	}
}

func TestControlKernelImageClientAuthenticatesPrivateDaemon(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("worker image transport fixture requires root")
	}
	claim := controlClaim()
	data := []byte("image data")
	session := &fixtureImageSession{fixtureReportSession: fixtureReportSession{claim: claim}, data: data}
	path, cancel, done := startReportControl(t, func(context.Context, *net.UnixConn, WorkerClaim) (reportSession, error) { return session, nil }, ControlLimits{Connections: 2, Reports: 1, Timeout: time.Second})
	defer func() { cancel(); _ = awaitControllerStop(t, done) }()
	var content bytes.Buffer
	if err := ReadImageAt(t.Context(), path, claim, imagePin(data), 1024, &content); err != nil || !bytes.Equal(content.Bytes(), data) {
		t.Fatal("private image read failed", err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := ReadImageAt(t.Context(), path, claim, imagePin(data), 1024, io.Discard); err == nil {
		t.Fatal("image reached unsafe endpoint")
	}
	if session.closes.Load() != 1 {
		t.Fatal("unsafe endpoint acquired service peer")
	}
}

func TestControlKernelImageServiceBindingAndAmbientRootDenial(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("worker image transport fixture requires root")
	}
	access, c, claim, _, source, root, _ := imageAccessFixture(t)
	limits := ControlLimits{Connections: 2, Reports: 1, Timeout: time.Second}
	for _, content := range []WorkerContent{{Images: &WorkerImages{controller: &Controller{}}}, {Evidence: &WorkerEvidence{controller: &Controller{}}, Images: access}} {
		if server, err := NewWorkerControl(c, limits, content); err == nil || server != nil {
			t.Fatal("foreign content controller accepted")
		}
	}
	server, err := NewWorkerControl(c, limits, WorkerContent{Images: access})
	if err != nil {
		t.Fatal(err)
	}
	listener, path := controlListener(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	if err = ReadImageAt(t.Context(), path, claim, root, 1<<20, io.Discard); err == nil {
		t.Fatal("ambient root read image")
	}
	cancel()
	_ = awaitControllerStop(t, done)
	if source.opens != 0 {
		t.Fatal("ambient root reached image source")
	}
}

func TestControlKernelOwnedEndpointJoinsAndAllowsRestart(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("owned endpoint fixture requires root")
	}
	controller, _, _ := controllerFixture(t)
	cancelController, controllerDone := runController(t, controller)
	defer func() { cancelController(); _ = awaitControllerStop(t, controllerDone) }()
	awaitControllerReady(t, controller)
	path := endpointPath(t)
	for attempt := 0; attempt < 2; attempt++ {
		server, err := NewWorkerControl(controller, ControlLimits{Connections: 2, Reports: 1, Timeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- server.ServeAt(ctx, path) }()
		deadline := time.Now().Add(5 * time.Second)
		for {
			err = AuthorizeAt(t.Context(), path, controlClaim())
			if errors.Is(err, broker.ErrDenied) {
				break
			}
			if time.Now().After(deadline) {
				cancel()
				_ = awaitControllerStop(t, done)
				t.Fatal("owned endpoint unavailable", err)
			}
			time.Sleep(time.Millisecond)
		}
		cancel()
		if err = awaitControllerStop(t, done); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if _, err = os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("owned endpoint retained after joined shutdown", err)
		}
	}
}
