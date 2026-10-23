package fabriqbrain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	log "github.com/xraph/go-utils/log"
	"github.com/xraph/vessel"

	"github.com/xraph/cortex/id"
)

// A failed memory write is logged by its error's type and the run's ID. The
// row it failed to write is the run's input and output, and the store's
// message quotes it, so none of that reaches the log. A second failure inside
// the interval is counted, not written.
func TestPluginLogsCarryNoRunContent(t *testing.T) {
	tl := log.NewTestLogger()
	capture, ok := tl.(*log.TestLogger)
	if !ok {
		t.Fatalf("NewTestLogger returned %T", tl)
	}
	rem := &fakeRememberer{err: errors.New(`insert refused: content "INPUT-CANARY alice@example.com"`)}
	p := NewPlugin(rem, WithLogger(tl))
	for range 2 {
		runID := id.NewAgentRunID()
		_ = p.OnRunStarted(context.Background(), id.NewAgentID(), runID, "INPUT-CANARY alice@example.com")
		_ = p.OnRunCompleted(context.Background(), id.NewAgentID(), runID, "OUTPUT-CANARY", 0)
	}

	logs := capture.GetLogs()
	if len(logs) != 1 || logs[0].Message != "fabriq-brain: memory write failed" {
		t.Fatalf("logs = %v, want one throttled memory write line", logs)
	}
	var b strings.Builder
	for _, f := range logs[0].Fields {
		fmt.Fprintf(&b, "%s=%v ", f.Key(), f.Value())
	}
	for _, c := range []string{"alice@example.com", "INPUT-CANARY", "OUTPUT-CANARY", "insert refused"} {
		if strings.Contains(b.String(), c) {
			t.Errorf("the log line carries %q: %s", c, b.String())
		}
	}
	if v, has := logs[0].Field("error_type"); !has || v != "*errors.errorString" {
		t.Errorf("error_type = %v, want the error's type", v)
	}
}

// In a Forge app the container holds the app's logger, and the bridge uses
// it unless you pass WithLogger yourself.
func TestHostLoggerIsUsedUnlessYouPassOne(t *testing.T) {
	host, mine := log.NewTestLogger(), log.NewTestLogger()
	c := vessel.New()
	if err := vessel.Provide(c, func() (log.Logger, error) { return host, nil }); err != nil {
		t.Fatalf("Provide logger: %v", err)
	}
	if got := applyOptions(withHostLogger(c, nil)).logger; got != host {
		t.Error("the bridge ignored the container's logger")
	}
	if got := applyOptions(withHostLogger(c, []Option{WithLogger(mine)})).logger; got != mine {
		t.Error("the container's logger overrode the one you passed")
	}
	if got := applyOptions(withHostLogger(vessel.New(), nil)).logger; got == nil {
		t.Error("an empty container left no logger at all")
	}
}
