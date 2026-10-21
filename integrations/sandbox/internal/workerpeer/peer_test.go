package workerpeer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/sandbox"
)

type fixtureProcess struct {
	key    id.ID
	live   bool
	status string
	group  string
	onRead func()
	closed int
}

func (p *fixtureProcess) pid() int32 { return 123 }
func (p *fixtureProcess) alive(context.Context) error {
	if !p.live {
		return errors.New("peer exited")
	}
	return nil
}
func (p *fixtureProcess) snapshot(context.Context) ([]byte, []byte, error) {
	if p.onRead != nil {
		p.onRead()
	}
	return []byte(p.status), []byte(p.group), nil
}
func (p *fixtureProcess) close() error { p.closed++; return nil }

func peerFixture() (*Peer, *fixtureProcess) {
	key := id.New(sandbox.PrefixJob)
	p := &fixtureProcess{key: key, live: true, status: "Name:\tworker\nTgid:\t123\nPid:\t123\nUid:\t0\t0\t0\t0\nGid:\t0\t0\t0\t0\n", group: "0::/system.slice/cortex-sandbox-worker@" + key.String() + ".service/supervisor\n"}
	return &Peer{key: key, process: p}, p
}

func TestPeerRequiresLiveRootProcessInExactWorkerSubgroup(t *testing.T) {
	p, fixture := peerFixture()
	pid, err := p.Check(t.Context(), p.key)
	if err != nil || pid != 123 {
		t.Fatalf("valid peer refused: %d %v", pid, err)
	}
	if foreign, checkErr := p.Check(t.Context(), id.New(sandbox.PrefixJob)); checkErr == nil || foreign != 0 {
		t.Fatal("peer handle authorized a different job")
	}
	fixture.group = strings.ReplaceAll(fixture.group, "/supervisor", "/payload")
	pid, err = p.Check(t.Context(), p.key)
	if err == nil || pid != 0 {
		t.Fatalf("payload authenticated as worker: %d %v", pid, err)
	}
}

func TestPeerRejectsDeathDuringIdentityRead(t *testing.T) {
	p, fixture := peerFixture()
	fixture.onRead = func() { fixture.live = false }
	pid, err := p.Check(t.Context(), p.key)
	if err == nil || pid != 0 {
		t.Fatalf("exited peer authenticated from stale proc data: %d %v", pid, err)
	}
}

func TestPeerRejectsChangedCredentialsAndForeignProcess(t *testing.T) {
	for _, status := range []string{
		"Tgid:\t123\nPid:\t123\nUid:\t0\t1000\t0\t0\nGid:\t0\t0\t0\t0\n",
		"Tgid:\t123\nPid:\t123\nUid:\t0\t0\t0\t0\nGid:\t0\t0\t1\t0\n",
		"Tgid:\t124\nPid:\t124\nUid:\t0\t0\t0\t0\nGid:\t0\t0\t0\t0\n",
		"Tgid:\t123\nPid:\t123\nUid:\t0\t0\t0\t0\n",
		"Tgid:\t123\nPid:\t123\nUid:\t0\t0\t0\t0\nGid:\t0\t0\t0\t0\nUid:\t0\t0\t0\t0\n",
		strings.Repeat("x", 65537),
	} {
		t.Run(fmt.Sprintf("status_%d", len(status)), func(t *testing.T) {
			p, fixture := peerFixture()
			fixture.status = status
			if pid, err := p.Check(t.Context(), p.key); err == nil || pid != 0 {
				t.Fatalf("invalid process status authenticated: %d %v", pid, err)
			}
		})
	}
}

func TestPeerCloseAndCancellationCannotRetainAuthority(t *testing.T) {
	p, fixture := peerFixture()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if pid, err := p.Check(ctx, p.key); !errors.Is(err, context.Canceled) || pid != 0 {
		t.Fatalf("cancelled check: %d %v", pid, err)
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			_ = p.Close()
			if pid, err := p.Check(t.Context(), p.key); err == nil || pid != 0 {
				t.Errorf("closed peer authenticated: %d %v", pid, err)
			}
		})
	}
	wg.Wait()
	if fixture.closed != 1 {
		t.Fatalf("descriptor closed %d times", fixture.closed)
	}
}
