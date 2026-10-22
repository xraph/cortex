// The isolation probe is a trusted qualification fixture, not generated user code.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	if err := probe(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func probe(args []string) error {
	switch args[0] {
	case "hello":
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"status": "ready"})
	case "output":
		block := make([]byte, 4096)
		for range 512 {
			if _, err := os.Stdout.Write(block); err != nil {
				return err
			}
			if _, err := os.Stderr.Write(block); err != nil {
				return err
			}
		}
		return fmt.Errorf("output limit was not enforced")
	case "sleep":
		time.Sleep(2 * time.Minute)
		return nil
	case "children":
		for range 3 {
			cmd := exec.CommandContext(context.Background(), "/probe", "sleep")
			if err := cmd.Start(); err != nil {
				return err
			}
		}
		if err := json.NewEncoder(os.Stdout).Encode(map[string]int{"children": 3}); err != nil {
			return err
		}
		time.Sleep(2 * time.Minute)
		return nil
	case "files":
		if len(args) != 2 {
			return fmt.Errorf("missing host sentinel")
		}
		for _, path := range []string{args[1], "/var/run/docker.sock", "/run/containerd/containerd.sock"} {
			if _, err := os.ReadFile(path); err == nil {
				return fmt.Errorf("host path readable: %s", path)
			}
		}
		if err := os.WriteFile("/root-write-probe", []byte("unexpected"), 0600); err == nil {
			return fmt.Errorf("root filesystem writable")
		}
		data, err := os.ReadFile("/inputs/protected")
		if err != nil {
			return err
		}
		if string(data) != "protected-input" {
			return fmt.Errorf("input content mismatch")
		}
		if err = os.WriteFile("/inputs/protected", []byte("unexpected"), 0600); err == nil {
			return fmt.Errorf("protected input writable")
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"status": "denied"})
	case "scratch":
		if len(args) != 2 {
			return fmt.Errorf("missing byte limit")
		}
		limit, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil || limit <= 0 || limit > 64<<20 {
			return fmt.Errorf("invalid byte limit")
		}
		file, err := os.Create("/tmp/fill")
		if err != nil {
			return err
		}
		defer file.Close()
		block := make([]byte, 4096)
		for written := int64(0); written <= limit; written += int64(len(block)) {
			if _, err = file.Write(block); err != nil {
				return json.NewEncoder(os.Stdout).Encode(map[string]string{"status": "limited"})
			}
		}
		return fmt.Errorf("scratch limit was not enforced")
	case "network":
		if len(args) != 3 {
			return fmt.Errorf("missing local fixture endpoints")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		dialer := net.Dialer{Timeout: time.Second}
		if conn, err := dialer.DialContext(ctx, "tcp", args[1]); err == nil {
			conn.Close()
			return fmt.Errorf("host HTTP fixture reachable")
		}
		resolver := net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "udp", args[2])
		}}
		if ips, err := resolver.LookupHost(ctx, "qualification.invalid"); err == nil || len(ips) != 0 {
			return fmt.Errorf("host DNS fixture reachable")
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"status": "denied"})
	default:
		return fmt.Errorf("unsupported fixture mode")
	}
}
