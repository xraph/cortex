//go:build linux && sandbox_peer_kernel

package workerpeer

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/sandbox"
)

func connectedChild(t *testing.T) (*net.UnixConn, *exec.Cmd) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Fatal("worker peer kernel tests require root")
	}
	directory, err := os.MkdirTemp("/tmp", "cortex-peer-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	address := filepath.Join(directory, "worker.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: address, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if err = listener.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	child := exec.CommandContext(ctx, executable, "-test.run=^TestPeerKernelChild$", "-test.count=1")
	child.Env = []string{"CORTEX_PEER_TEST_SOCKET=" + address}
	child.Stdout, child.Stderr = io.Discard, os.Stderr
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	connection, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return connection, child
}

func TestPeerKernelChild(t *testing.T) {
	address := os.Getenv("CORTEX_PEER_TEST_SOCKET")
	if address == "" {
		return
	}
	connection, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: address, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	var body [1]byte
	_, err = connection.Read(body[:])
	if !errors.Is(err, io.EOF) {
		t.Fatalf("unexpected worker test message: %v", err)
	}
}

func TestPeerKernelPinsSocketProcessAndDetectsExit(t *testing.T) {
	connection, child := connectedChild(t)
	process, err := openProcess(t.Context(), connection)
	if err != nil {
		t.Fatal(err)
	}
	defer process.close()
	if process.pid() != int32(child.Process.Pid) {
		t.Fatalf("socket peer was not connecting child: %d", process.pid())
	}
	if err = process.alive(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, membership, err := process.snapshot(t.Context())
	if err != nil || len(membership) == 0 {
		t.Fatalf("procfs snapshot: %v", err)
	}
	if err = checkStatus(status, int32(child.Process.Pid)); err != nil {
		t.Fatal(err)
	}
	if err = connection.Close(); err != nil {
		t.Fatal(err)
	}
	if err = child.Wait(); err != nil {
		t.Fatal(err)
	}
	if err = process.alive(t.Context()); err == nil {
		t.Fatal("exited socket peer retained authority")
	}
}

func TestPeerKernelRejectsAmbientRootAndDoesNotLeakHandles(t *testing.T) {
	connection, _ := connectedChild(t)
	key := id.New(sandbox.PrefixJob)
	// This root child is deliberately outside a job service. Real kernel
	// credentials alone must not authorize it as a worker.
	if peer, err := Open(t.Context(), connection, key); err == nil || peer != nil {
		t.Fatal("ambient root authenticated as job worker")
	}
	before, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		if peer, openErr := Open(t.Context(), connection, key); openErr == nil || peer != nil {
			t.Fatal("uncontained peer accepted")
		}
	}
	after, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) > len(before) {
		t.Fatalf("rejected peers leaked kernel handles: before=%d after=%d", len(before), len(after))
	}
}
