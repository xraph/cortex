//go:build sandbox_qualification

package sandboxbroker_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/sandbox/deployment"
	"github.com/xraph/cortex/integrations/sandbox/internal/supervisor"
	"github.com/xraph/cortex/integrations/sandbox/oci"
	"github.com/xraph/cortex/sandbox"
)

// These tests deliberately fail when the required worker or binaries are absent.
// They run only trusted, source-controlled fixtures. Passing this subset does not
// qualify broker crash recovery, output supervision or protected grading.
func TestLiveIsolationQualification(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Fatal("qualification blocked: a Linux worker is required")
	}
	if os.Geteuid() != 0 {
		t.Fatal("qualification blocked: this profile requires a dedicated root-run Linux worker")
	}
	runsc := requiredBinary(t, "CORTEX_QUALIFICATION_RUNSC", "CORTEX_QUALIFICATION_RUNSC_SHA256")
	manifest, deploymentDigest := qualifiedDeployment(t, runsc)
	fixture := requiredBinary(t, "CORTEX_QUALIFICATION_PROBE", "CORTEX_QUALIFICATION_PROBE_SHA256")
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		t.Fatalf("qualification blocked: cgroup v2 required: %v", err)
	}
	version := command(t, runsc, "--version")
	t.Logf("deployment SHA256=%s runtime SHA256=%s version=%s fixture SHA256=%s", deploymentDigest, os.Getenv("CORTEX_QUALIFICATION_RUNSC_SHA256"), bytes.TrimSpace(version), os.Getenv("CORTEX_QUALIFICATION_PROBE_SHA256"))
	for _, mode := range []string{"hello", "output", "children"} {
		t.Run("Supervised_"+mode, func(t *testing.T) {
			h := stageProbe(t, fixture, mode)
			driver, err := supervisor.NewRunsc(t.Context(), supervisor.RunscConfig{Binary: runsc, StateRoot: h.root, Bundle: h.bundle, Rootfs: filepath.Join(h.bundle, "rootfs"), Inputs: map[string]string{"/inputs/protected": h.input}, JobDigest: h.job.Digest, Manifest: manifest, DeploymentDigest: deploymentDigest})
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(h.job.Limits.WallTime)
			if mode == "children" {
				deadline = time.Now().Add(5 * time.Second)
			}
			result, runErr := supervisor.Run(t.Context(), driver, h.job, deadline)
			switch mode {
			case "hello":
				if runErr != nil || result.State != sandbox.StateExited || result.ExitCode != 0 || !bytes.Contains(result.Stdout, []byte("ready")) {
					t.Fatalf("supervised baseline: %+v %v", result, runErr)
				}
			case "output":
				if !errors.Is(runErr, supervisor.ErrOutputLimit) || result.State != sandbox.StateFailed || int64(len(result.Stdout)+len(result.Stderr)) != h.job.Limits.OutputBytes {
					t.Fatalf("supervised output: state=%s bytes=%d error=%v", result.State, len(result.Stdout)+len(result.Stderr), runErr)
				}
			case "children":
				if !errors.Is(runErr, context.DeadlineExceeded) || result.State != sandbox.StateFailed || !bytes.Contains(result.Stdout, []byte("children")) {
					t.Fatalf("supervised deadline: %+v %v", result, runErr)
				}
			}
			events, readErr := os.ReadFile(filepath.Join("/sys/fs/cgroup/cortex-sandbox", h.key, "cgroup.events"))
			if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
				t.Fatal(readErr)
			}
			if bytes.Contains(events, []byte("populated 1")) {
				t.Fatal("supervised workload still has live cgroup descendants")
			}
		})
	}
	t.Run("Baseline", func(t *testing.T) {
		h := newProbe(t, runsc, fixture, "hello")
		h.start(t)
		h.expectExit(t, 0)
		h.expectOutput(t, "ready")
	})
	t.Run("HostAndInputProtection", func(t *testing.T) {
		host := filepath.Join(t.TempDir(), "host-secret")
		if err := os.WriteFile(host, []byte("private-host-sentinel"), 0600); err != nil {
			t.Fatal(err)
		}
		h := newProbe(t, runsc, fixture, "files", host)
		h.start(t)
		h.expectExit(t, 0)
		h.expectOutput(t, "denied")
		data, err := os.ReadFile(h.input)
		if err != nil || string(data) != "protected-input" {
			t.Fatalf("host input changed: %q %v", data, err)
		}
	})
	t.Run("ScratchLimit", func(t *testing.T) {
		h := newProbe(t, runsc, fixture, "scratch", strconv.FormatInt(8<<20, 10))
		h.start(t)
		h.expectExit(t, 0)
		h.expectOutput(t, "limited")
	})
	t.Run("NetworkDNSAndRedirectDenial", func(t *testing.T) {
		var httpRequests, dnsPackets atomic.Int64
		host := privateWorkerAddress(t)
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			httpRequests.Add(1)
			http.Redirect(w, r, "http://127.0.0.1:9/redirect-target", http.StatusFound)
		}))
		listener, listenErr := new(net.ListenConfig).Listen(t.Context(), "tcp", net.JoinHostPort(host, "0"))
		if listenErr != nil {
			t.Fatal(listenErr)
		}
		if closeErr := server.Listener.Close(); closeErr != nil {
			listener.Close()
			t.Fatal(closeErr)
		}
		server.Listener = listener
		server.Start()
		defer server.Close()
		conn, err := new(net.ListenConfig).ListenPacket(t.Context(), "udp", net.JoinHostPort(host, "0"))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		done := make(chan struct{})
		go func() {
			defer close(done)
			buffer := make([]byte, 4096)
			for {
				if _, _, readErr := conn.ReadFrom(buffer); readErr != nil {
					return
				}
				dnsPackets.Add(1)
			}
		}()
		h := newProbe(t, runsc, fixture, "network", strings.TrimPrefix(server.URL, "http://"), conn.LocalAddr().String())
		h.start(t)
		h.expectExit(t, 0)
		h.expectOutput(t, "denied")
		if err = conn.Close(); err != nil {
			t.Fatal(err)
		}
		<-done
		if httpRequests.Load() != 0 || dnsPackets.Load() != 0 {
			t.Fatalf("egress reached host fixtures: HTTP=%d DNS=%d", httpRequests.Load(), dnsPackets.Load())
		}
	})
	t.Run("EnforcedCgroupLimits", func(t *testing.T) {
		h := newProbe(t, runsc, fixture, "sleep")
		h.start(t)
		pid := h.pid(t)
		group := unifiedCgroup(t, pid)
		for name, want := range map[string]string{"memory.max": "268435456", "pids.max": "128", "cpu.max": "50000 100000"} {
			got, err := os.ReadFile(filepath.Join(group, name))
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(string(got)) != want {
				t.Fatalf("%s=%q; expected %q", name, got, want)
			}
		}
		h.kill(t)
		h.expectExit(t, 137)
	})
	t.Run("CancelKillsDescendants", func(t *testing.T) {
		h := newProbe(t, runsc, fixture, "children")
		h.start(t)
		h.awaitOutput(t, "children")
		before := h.processes(t)
		if len(before) < 4 {
			t.Fatalf("fixture did not create children: %v", before)
		}
		h.kill(t)
		h.expectExit(t, 137)
		after := h.processes(t)
		if len(after) != 0 {
			t.Fatalf("descendants survived SIGKILL: %v", after)
		}
	})
	t.Run("ReopenRuntimeState", func(t *testing.T) {
		h := newProbe(t, runsc, fixture, "sleep")
		h.start(t)
		original := h.pid(t)
		reopened := *h
		reopened.output = nil
		if got := reopened.pid(t); got != original {
			t.Fatalf("runtime identity changed: %d -> %d", original, got)
		}
		reopened.kill(t)
		h.expectExit(t, 137)
	})
}

func qualifiedDeployment(t *testing.T, runsc string) (deployment.Manifest, string) {
	t.Helper()
	manifestPath := os.Getenv("CORTEX_QUALIFICATION_RUNTIME_MANIFEST")
	if !filepath.IsAbs(manifestPath) || filepath.Base(runsc) != "runsc" {
		t.Fatal("qualification blocked: an absolute deployment manifest and a dedicated runsc installation are required")
	}
	file, err := os.Open(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	if err != nil || len(body) > 64<<10 {
		t.Fatalf("qualification blocked: invalid deployment manifest: %v", err)
	}
	var manifest deployment.Manifest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&manifest); err != nil {
		t.Fatal(err)
	}
	if err = decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		t.Fatal("qualification blocked: trailing deployment manifest data")
	}
	digest, err := deployment.Verify(t.Context(), filepath.Dir(runsc), manifest, 0)
	if err != nil {
		t.Fatalf("qualification blocked: runtime deployment verification: %v", err)
	}
	return manifest, digest
}

func requiredBinary(t *testing.T, pathKey, digestKey string) string {
	t.Helper()
	path := os.Getenv(pathKey)
	want := os.Getenv(digestKey)
	if !filepath.IsAbs(path) || !evidence.ValidDigest(want) {
		t.Fatalf("qualification blocked: %s and lowercase SHA256 %s are required", pathKey, digestKey)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || info.Mode().Perm()&0022 != 0 {
		t.Fatal("qualification binary must be a regular executable without group/world write")
	}
	hash := sha256.New()
	if _, err = io.Copy(hash, file); err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != want {
		t.Fatalf("qualification binary digest mismatch: %s", path)
	}
	return path
}
func command(t *testing.T, binary string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.WaitDelay = time.Second
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("runtime command %v: %v: %s", args, err, output)
	}
	return output
}

type liveProbe struct {
	binary, root, key, input string
	bundle                   string
	job                      sandbox.Job
	output                   *os.File
}

func stageProbe(t *testing.T, fixture string, args ...string) *liveProbe {
	t.Helper()
	base := t.TempDir()
	rootfs := filepath.Join(base, "rootfs")
	for _, name := range []string{rootfs, filepath.Join(rootfs, "tmp"), filepath.Join(rootfs, "proc"), filepath.Join(rootfs, "inputs")} {
		if err := os.Mkdir(name, 0755); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(rootfs, "probe"), data, 0555); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(base, "input")
	if err = os.WriteFile(input, []byte("protected-input"), 0444); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(rootfs, "inputs", "protected"), nil, 0444); err != nil {
		t.Fatal(err)
	}
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "qualification", Value: "local"}}}
	now := time.Now().UTC()
	artifact, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: id.New(evidence.PrefixArtifact), Scope: scope, MediaType: "text/plain", Version: "fixture-v1", Producer: "qualification", Sensitivity: evidence.SensitivityInternal, CreatedAt: now, RetainUntil: now.Add(time.Hour)}, Data: []byte("protected-input")})
	if err != nil {
		t.Fatal(err)
	}
	job, err := sandbox.Seal(sandbox.Job{SchemaVersion: 1, ID: id.New(sandbox.PrefixJob), Key: "qualification", Scope: scope, Principal: "qualification-supervisor", Image: "sha256:" + evidence.Digest(data), Argv: append([]string{"/probe"}, args...), Inputs: []sandbox.Input{{Path: "/inputs/protected", Ref: artifact.Ref}}, Network: sandbox.NetworkDenied, Limits: sandbox.Limits{CPUMillis: 500, MemoryBytes: 256 << 20, Processes: 128, ScratchBytes: 8 << 20, WallTime: 30 * time.Second, OutputBytes: 1 << 20}})
	if err != nil {
		t.Fatal(err)
	}
	spec, err := oci.Spec(job, rootfs, map[string]string{"/inputs/protected": input})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(base, "config.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "runtime")
	if err = os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	return &liveProbe{root: root, key: job.ID.String(), input: input, bundle: base, job: job}
}

func newProbe(t *testing.T, binary, fixture string, args ...string) *liveProbe {
	h := stageProbe(t, fixture, args...)
	h.binary = binary
	base := h.bundle
	out, err := os.OpenFile(filepath.Join(base, "stdout"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	errFile, err := os.OpenFile(filepath.Join(base, "stderr"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		out.Close()
		t.Fatal(err)
	}
	h.output = out
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, h.args("delete", "--force", h.key)...)
		cmd.WaitDelay = time.Second
		if output, deleteErr := cmd.CombinedOutput(); deleteErr != nil {
			t.Errorf("runtime cleanup failed: %v: %s", deleteErr, output)
		}
		out.Close()
		errFile.Close()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, h.args("create", "--bundle", base, h.key)...)
	cmd.Stdout = out
	cmd.Stderr = errFile
	cmd.WaitDelay = time.Second
	if err = cmd.Run(); err != nil {
		diagnostic, _ := os.ReadFile(filepath.Join(base, "stderr"))
		t.Fatalf("create: %v: %s", err, diagnostic)
	}
	return h
}
func (h *liveProbe) args(args ...string) []string {
	return append([]string{"--root=" + h.root, "--network=none"}, args...)
}
func (h *liveProbe) start(t *testing.T) { t.Helper(); command(t, h.binary, h.args("start", h.key)...) }
func (h *liveProbe) kill(t *testing.T) {
	t.Helper()
	command(t, h.binary, h.args("kill", "--all", h.key, "SIGKILL")...)
}
func (h *liveProbe) expectExit(t *testing.T, want int) {
	t.Helper()
	var result struct {
		ID         string `json:"id"`
		ExitStatus int    `json:"exitStatus"`
	}
	if err := json.Unmarshal(command(t, h.binary, h.args("wait", h.key)...), &result); err != nil {
		t.Fatal(err)
	}
	if result.ID != h.key || result.ExitStatus != want {
		t.Fatalf("exit %+v, expected %d; output %s", result, want, h.readOutput(t))
	}
}
func (h *liveProbe) pid(t *testing.T) int {
	t.Helper()
	var state struct {
		ID     string `json:"id"`
		PID    int    `json:"pid"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(command(t, h.binary, h.args("state", h.key)...), &state); err != nil {
		t.Fatal(err)
	}
	if state.ID != h.key || state.PID <= 0 || state.Status != "running" {
		t.Fatalf("unexpected runtime state %+v", state)
	}
	return state.PID
}
func (h *liveProbe) processes(t *testing.T) []int {
	t.Helper()
	var pids []int
	if err := json.Unmarshal(command(t, h.binary, h.args("ps", "--format=json", h.key)...), &pids); err != nil {
		t.Fatal(err)
	}
	return pids
}
func (h *liveProbe) readOutput(t *testing.T) []byte {
	t.Helper()
	info, err := h.output.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 1<<20 {
		t.Fatal("qualification fixture exceeded output bound")
	}
	data := make([]byte, info.Size())
	_, err = h.output.ReadAt(data, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	return data
}
func (h *liveProbe) expectOutput(t *testing.T, status string) {
	t.Helper()
	var output map[string]string
	if err := json.Unmarshal(h.readOutput(t), &output); err != nil {
		t.Fatal(err)
	}
	if output["status"] != status {
		t.Fatalf("unexpected output %v", output)
	}
}
func (h *liveProbe) awaitOutput(t *testing.T, marker string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if bytes.Contains(h.readOutput(t), []byte(marker)) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("fixture did not reach %s", marker)
}
func unifiedCgroup(t *testing.T, pid int) string {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if value, ok := strings.CutPrefix(line, "0::/"); ok {
			if value == "" || filepath.Clean(value) != value || strings.HasPrefix(value, "..") {
				t.Fatalf("unexpected cgroup path %q", value)
			}
			return filepath.Join("/sys/fs/cgroup", value)
		}
	}
	t.Fatal("unified cgroup not found")
	return ""
}

func privateWorkerAddress(t *testing.T) string {
	t.Helper()
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range addresses {
		ip, _, parseErr := net.ParseCIDR(address.String())
		if parseErr == nil && ip.To4() != nil && ip.IsPrivate() && !ip.IsLoopback() {
			return ip.String()
		}
	}
	t.Fatal("qualification blocked: no private worker interface for local egress fixtures")
	return ""
}
