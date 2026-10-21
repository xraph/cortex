// Package workerpeer authenticates local worker process identity.
package workerpeer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/sandbox/internal/containment"
)

type peerProcess interface {
	pid() int32
	alive(context.Context) error
	snapshot(context.Context) ([]byte, []byte, error)
	close() error
}

// Peer owns a kernel reference to one connected worker process. Its identity
// does not grant job authority: verify the durable claim, current grants and
// effective service configuration before accepting worker operations.
type Peer struct {
	mu       sync.Mutex
	key      id.ID
	process  peerProcess
	closed   bool
	closeErr error
}

// Open captures the connected process from a Linux Unix stream socket and
// verifies its job subgroup. It never resolves identity from a caller-supplied
// PID or falls back to a numeric-PID handle. The caller retains socket ownership.
func Open(ctx context.Context, connection *net.UnixConn, key id.ID) (*Peer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := containment.ForJob(key); err != nil {
		return nil, err
	}
	if connection == nil {
		return nil, fmt.Errorf("cortex: missing worker connection")
	}
	process, err := openProcess(ctx, connection)
	if err != nil {
		return nil, err
	}
	peer := &Peer{key: key, process: process}
	if _, err = peer.Check(ctx, key); err != nil {
		return nil, errors.Join(err, peer.Close())
	}
	return peer, nil
}

// Check verifies liveness around the procfs reads, current root credentials and
// exact job subgroup. The returned PID is only valid alongside this live handle;
// never cache it as an authorization decision or reopen identity by numeric PID.
func (p *Peer) Check(ctx context.Context, key id.ID) (uint32, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if p.closed || p.process == nil || key != p.key {
		return 0, fmt.Errorf("cortex: worker peer is closed or invalid")
	}
	pid := p.process.pid()
	if pid <= 1 {
		return 0, fmt.Errorf("cortex: invalid worker process identifier")
	}
	if err := p.process.alive(ctx); err != nil {
		return 0, err
	}
	status, membership, err := p.process.snapshot(ctx)
	if err != nil {
		return 0, err
	}
	if checkErr := checkStatus(status, pid); checkErr != nil {
		return 0, checkErr
	}
	if checkErr := containment.CheckMembership(p.key, membership); checkErr != nil {
		return 0, checkErr
	}
	if checkErr := errors.Join(p.process.alive(ctx), ctx.Err()); checkErr != nil {
		return 0, checkErr
	}
	return uint32(pid), nil
}

// Close releases the peer reference.
func (p *Peer) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		p.closed = true
		if p.process != nil {
			p.closeErr = p.process.close()
		}
	}
	return p.closeErr
}

func checkStatus(body []byte, pid int32) error {
	if len(body) == 0 || len(body) > 65536 || !utf8.Valid(body) || strings.ContainsRune(string(body), '\x00') {
		return fmt.Errorf("cortex: invalid worker process status")
	}
	expected := map[string]string{"Pid:": strconv.FormatInt(int64(pid), 10), "Tgid:": strconv.FormatInt(int64(pid), 10), "Uid:": "0 0 0 0", "Gid:": "0 0 0 0"}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		want, relevant := expected[fields[0]]
		if !relevant {
			continue
		}
		if seen[fields[0]] || strings.Join(fields[1:], " ") != want {
			return fmt.Errorf("cortex: worker process identity mismatch")
		}
		seen[fields[0]] = true
	}
	if len(seen) != len(expected) {
		return fmt.Errorf("cortex: incomplete worker process identity")
	}
	return nil
}
