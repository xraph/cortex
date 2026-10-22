package service

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"testing"

	"golang.org/x/sys/unix"
)

func TestControlEndpointCrashHelper(t *testing.T) {
	path := os.Getenv("CORTEX_ENDPOINT_CRASH_FIXTURE")
	if path == "" {
		return
	}
	if _, err := openControlEndpoint(t.Context(), path, uint32(os.Getuid())); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}

func TestControlEndpointRecoversRecordedProcessCrash(t *testing.T) {
	path := endpointPath(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.CommandContext(t.Context(), binary, "-test.run=^TestControlEndpointCrashHelper$")
	child.Env = append(os.Environ(), "CORTEX_ENDPOINT_CRASH_FIXTURE="+path)
	if out, runErr := child.CombinedOutput(); runErr != nil {
		t.Fatalf("crash fixture: %v %s", runErr, out)
	}
	if _, err = os.Lstat(path); err != nil {
		t.Fatal("crash did not retain socket", err)
	}
	endpoint, err := openControlEndpoint(t.Context(), path, uint32(os.Getuid()))
	if err != nil {
		t.Fatal("recorded socket was not recovered", err)
	}
	if err = endpoint.Close(); err != nil {
		t.Fatal(err)
	}
}

func abandonEndpoint(t *testing.T, e *controlEndpoint, keepListener bool) {
	t.Helper()
	if !keepListener {
		if err := e.listener.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		t.Cleanup(func() { _ = e.listener.Close() })
	}
	if err := e.lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := e.directory.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestControlEndpointRecoveryRefusesLiveOrChangedSocket(t *testing.T) {
	for _, mode := range []string{"live", "replacement", "mode", "identity", "corrupt", "duplicate", "oversized", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			path := endpointPath(t)
			endpoint, err := openControlEndpoint(t.Context(), path, uint32(os.Getuid()))
			if err != nil {
				t.Fatal(err)
			}
			abandonEndpoint(t, endpoint, mode == "live")
			switch mode {
			case "replacement":
				os.Remove(path)
				replacement, listenErr := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
				if listenErr != nil {
					t.Fatal(listenErr)
				}
				replacement.SetUnlinkOnClose(false)
				replacement.Close()
				os.Chmod(path, 0o600)
			case "mode":
				os.Chmod(path, 0o666)
			case "identity":
				body, readErr := os.ReadFile(path + ".lock")
				if readErr != nil {
					t.Fatal(readErr)
				}
				var record controlSocketRecord
				if err = json.Unmarshal(body, &record); err != nil {
					t.Fatal(err)
				}
				record.Inode++
				body, err = json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				os.WriteFile(path+".lock", body, 0o600)
			case "corrupt":
				os.WriteFile(path+".lock", []byte("{"), 0o600)
			case "oversized":
				os.WriteFile(path+".lock", bytes.Repeat([]byte("x"), 4097), 0o600)
			case "duplicate", "unknown":
				body, readErr := os.ReadFile(path + ".lock")
				if readErr != nil {
					t.Fatal(readErr)
				}
				extra := []byte(`,"schema_version":1}`)
				if mode == "unknown" {
					extra = []byte(`,"unknown":true}`)
				}
				os.WriteFile(path+".lock", append(body[:len(body)-1], extra...), 0o600)
			}
			if recovered, recoverErr := openControlEndpoint(t.Context(), path, uint32(os.Getuid())); recoverErr == nil || recovered != nil {
				t.Fatal("unproven stale socket removed")
			}
			if _, err = os.Lstat(path); err != nil {
				t.Fatal("refused socket removed", err)
			}
		})
	}
}

func TestControlSocketRecordBindsMetadata(t *testing.T) {
	stat := unix.Stat_t{Dev: 1, Ino: 2, Uid: 3, Gid: 4, Mode: unix.S_IFSOCK | 0o600, Nlink: 1, Ctim: unix.Timespec{Sec: 5, Nsec: 6}}
	original := newControlSocketRecord("/tmp/control.sock", stat)
	for _, mutate := range []func(*unix.Stat_t){func(s *unix.Stat_t) { s.Ctim.Nsec++ }, func(s *unix.Stat_t) { s.Ctim.Sec++ }, func(s *unix.Stat_t) { s.Ino++ }, func(s *unix.Stat_t) { s.Dev++ }, func(s *unix.Stat_t) { s.Uid++ }, func(s *unix.Stat_t) { s.Gid++ }, func(s *unix.Stat_t) { s.Mode = unix.S_IFREG | 0o600 }, func(s *unix.Stat_t) { s.Nlink++ }} {
		changed := stat
		mutate(&changed)
		if original == newControlSocketRecord("/tmp/control.sock", changed) {
			t.Fatal("metadata change lost")
		}
	}
}
