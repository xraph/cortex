package shield

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xraph/cortex/safety"

	"github.com/xraph/shield"
	"github.com/xraph/shield/presets"
)

// segmentRequest is the request the engine opens a model-output segment
// checker with.
func segmentRequest(profile string) safety.PublicationRequest {
	return safety.PublicationRequest{
		ScanRequest: safety.ScanRequest{Direction: safety.DirectionOutput, ProfileName: profile},
		Source:      "model_output",
	}
}

// newChecker opens a segment checker for model output under profile.
func newChecker(ctx context.Context, t *testing.T, a *Adapter, profile string) safety.SegmentChecker {
	t.Helper()
	c, err := a.NewSegmentChecker(ctx, segmentRequest(profile))
	if err != nil {
		t.Fatalf("NewSegmentChecker: %v", err)
	}
	if c == nil {
		t.Fatal("NewSegmentChecker returned a nil checker")
	}
	return c
}

func TestAdapterIsIncremental(t *testing.T) {
	eng, _ := seededEngine(t)
	var gate safety.IncrementalGate = New(eng)
	c, err := gate.NewSegmentChecker(scoped(testTenant), segmentRequest(presets.InfraAgentOutput))
	if err != nil || c == nil {
		t.Fatalf("NewSegmentChecker = %v, %v; want a checker", c, err)
	}
}

func TestSegmentCutsAtSentenceAndLineEnds(t *testing.T) {
	eng, _ := seededEngine(t)
	a := New(eng, WithApp(testApp))
	ctx := scoped(testTenant)
	cases := []struct {
		pending string
		want    string
	}{
		{"Deployed api. Next I", "Deployed api. "},
		{"line one\nline tw", "line one\n"},
		{"no end yet", ""},
		{"Done! Now? Next: step", "Done! Now? Next: "},
		{"version 1.2 is live", ""},
		{"see https://example.org/x", ""},
		{"one.\ntwo. three", "one.\ntwo. "},
	}
	for _, tc := range cases {
		t.Run(tc.pending, func(t *testing.T) {
			decision, consumed, err := newChecker(ctx, t, a, presets.InfraAgentOutput).CheckSegment(ctx, tc.pending, false)
			if err != nil {
				t.Fatalf("CheckSegment: %v", err)
			}
			if consumed != len(tc.want) || decision.Text != tc.want {
				t.Fatalf("consumed %d, text %q; want %d, %q", consumed, decision.Text, len(tc.want), tc.want)
			}
			if tc.want != "" && decision.Status != safety.StatusApproved {
				t.Fatalf("status %q, want approved", decision.Status)
			}
		})
	}
}

func TestSegmentCutsLongRunsAtWhitespace(t *testing.T) {
	eng, _ := seededEngine(t)
	a := New(eng, WithApp(testApp), WithSegmentLimit(64))
	ctx := scoped(testTenant)

	// 80 bytes of words with no sentence end, ending inside a word.
	pending := strings.Repeat("alpha beta gamma ", 5)[:80]
	if len(pending) != 80 || pending[79] == ' ' {
		t.Fatalf("fixture %q is %d bytes", pending, len(pending))
	}
	decision, consumed, err := newChecker(ctx, t, a, presets.InfraAgentOutput).CheckSegment(ctx, pending, false)
	if err != nil {
		t.Fatalf("CheckSegment: %v", err)
	}
	want := strings.LastIndexByte(pending, ' ') + 1
	if consumed != want || pending[consumed-1] != ' ' || decision.Text != pending[:consumed] {
		t.Fatalf("consumed %d, text %q; want the %d bytes up to the last space", consumed, decision.Text, want)
	}

	// Under the limit the run is held.
	short := pending[:63]
	decision, consumed, err = newChecker(ctx, t, a, presets.InfraAgentOutput).CheckSegment(ctx, short, false)
	if err != nil || consumed != 0 || decision.Text != "" {
		t.Fatalf("under the limit: consumed %d, text %q, err %v; want a hold", consumed, decision.Text, err)
	}

	// One word over the limit has nowhere to cut, so it is held whole.
	word := strings.Repeat("x", 100)
	decision, consumed, err = newChecker(ctx, t, a, presets.InfraAgentOutput).CheckSegment(ctx, word, false)
	if err != nil || consumed != 0 || decision.Text != "" {
		t.Fatalf("one long word: consumed %d, text %q, err %v; want a hold", consumed, decision.Text, err)
	}
}

func TestFinalConsumesEverything(t *testing.T) {
	eng, _ := seededEngine(t)
	a := New(eng, WithApp(testApp))
	ctx := scoped(testTenant)
	decision, consumed, err := newChecker(ctx, t, a, presets.InfraAgentOutput).CheckSegment(ctx, "tail", true)
	if err != nil {
		t.Fatalf("CheckSegment: %v", err)
	}
	if consumed != 4 || decision.Text != "tail" || decision.Status != safety.StatusApproved {
		t.Fatalf("consumed %d, decision %+v; want 4 and tail approved", consumed, decision)
	}

	// A final check with nothing pending settles the stream without a scan.
	decision, consumed, err = newChecker(ctx, t, a, presets.InfraAgentOutput).CheckSegment(ctx, "", true)
	if err != nil || consumed != 0 || decision.Text != "" || decision.Status != safety.StatusApproved {
		t.Fatalf("empty final: consumed %d, decision %+v, err %v", consumed, decision, err)
	}
}

func TestSegmentRedactsSecrets(t *testing.T) {
	eng, _ := seededEngine(t)
	a := New(eng, WithApp(testApp))
	ctx := scoped(testTenant)
	const pending = "Your key is AKIAIOSFODNN7EXAMPLE.\n"
	decision, consumed, err := newChecker(ctx, t, a, presets.InfraAgentOutput).CheckSegment(ctx, pending, false)
	if err != nil {
		t.Fatalf("CheckSegment: %v", err)
	}
	if consumed != len(pending) || decision.Status != safety.StatusRedacted || strings.Contains(decision.Text, "AKIAIOSFODNN7EXAMPLE") {
		t.Fatalf("consumed %d, decision %+v; want the whole line redacted", consumed, decision)
	}
}

func TestSegmentBlocks(t *testing.T) {
	eng, _ := seededEngine(t)
	a := New(eng, WithApp(testApp))
	ctx := scoped(testTenant)
	c := newChecker(ctx, t, a, strictTools)

	pending := "Pushed with " + githubToken() + " just now.\nMore"
	decision, consumed, err := c.CheckSegment(ctx, pending, false)
	if !errors.Is(err, safety.ErrBlocked) || decision.Text != "" || consumed != 0 {
		t.Fatalf("decision %+v, consumed %d, err %v; want safety.ErrBlocked and no text", decision, consumed, err)
	}

	// A blocked stream stays blocked: the held tail is never approved.
	decision, consumed, err = c.CheckSegment(ctx, "More", true)
	if err == nil || decision.Text != "" || consumed != 0 {
		t.Fatalf("after a block: decision %+v, consumed %d, err %v; want an error and no text", decision, consumed, err)
	}
}

func TestSegmentScanErrorFailsClosed(t *testing.T) {
	eng, _ := seededEngine(t)
	a := New(eng, WithApp(testApp))
	token := githubToken()
	pending := "The token " + token + " works.\n"

	c := newChecker(scoped("o9"), t, a, presets.InfraAgentOutput)
	decision, consumed, err := c.CheckSegment(scoped("o9"), pending, false)
	if !errors.Is(err, safety.ErrUnavailable) || !errors.Is(err, shield.ErrNoProfile) || decision.Text != "" || consumed != 0 {
		t.Fatalf("unseeded tenant: decision %+v, consumed %d, err %v; want safety.ErrUnavailable and no text", decision, consumed, err)
	}
	for _, leak := range []string{pending, token, "The token", presets.InfraAgentOutput, "o9"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("error %q leaks %q", err, leak)
		}
	}
	// A failed stream stays failed, even on its final check.
	if decision, consumed, err = c.CheckSegment(scoped(testTenant), "fine", true); err == nil || decision.Text != "" || consumed != 0 {
		t.Fatalf("after a failure: decision %+v, consumed %d, err %v", decision, consumed, err)
	}

	cancelled, cancel := context.WithCancel(scoped(testTenant))
	cancel()
	oversized := strings.Repeat("a ", shield.DefaultMaxScanBytes/2+1)
	cases := []struct {
		name    string
		ctx     context.Context
		profile string
		pending string
		final   bool
		want    error
	}{
		{"no profile", scoped(testTenant), "", pending, false, shield.ErrNoProfile},
		{"invalid profile", scoped(testTenant), brokenProfile, pending, false, shield.ErrProfileInvalid},
		{"over shield's limit", scoped(testTenant), presets.InfraAgentOutput, oversized, true, shield.ErrInputTooLarge},
		{"cancelled", cancelled, presets.InfraAgentOutput, pending, false, context.Canceled},
		{"cancelled hold", cancelled, presets.InfraAgentOutput, "no end yet", false, context.Canceled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newChecker(scoped(testTenant), t, a, tc.profile)
			decision, consumed, err := c.CheckSegment(tc.ctx, tc.pending, tc.final)
			if !errors.Is(err, tc.want) || decision.Text != "" || consumed != 0 {
				t.Fatalf("decision %+v, consumed %d, err %v; want %v and no text", decision, consumed, err, tc.want)
			}
			if !errors.Is(tc.want, context.Canceled) && !errors.Is(err, safety.ErrUnavailable) {
				t.Fatalf("err %v, want safety.ErrUnavailable", err)
			}
		})
	}
}

func TestSegmentCheckerRefusesWhatItCannotCheck(t *testing.T) {
	eng, _ := seededEngine(t)
	a := New(eng, WithApp(testApp))
	cancelled, cancel := context.WithCancel(scoped(testTenant))
	cancel()
	input := segmentRequest(presets.InfraAgentOutput)
	input.Direction = safety.DirectionInput
	cases := []struct {
		name string
		a    *Adapter
		ctx  context.Context
		req  safety.PublicationRequest
		want error
	}{
		{"no scope", a, context.Background(), segmentRequest(presets.InfraAgentOutput), shield.ErrTenantRequired},
		{"no engine", New(nil), scoped(testTenant), segmentRequest(presets.InfraAgentOutput), safety.ErrUnavailable},
		{"nil adapter", nil, scoped(testTenant), segmentRequest(presets.InfraAgentOutput), safety.ErrUnavailable},
		{"input direction", a, scoped(testTenant), input, safety.ErrUnavailable},
		{"cancelled", a, cancelled, segmentRequest(presets.InfraAgentOutput), context.Canceled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := tc.a.NewSegmentChecker(tc.ctx, tc.req)
			if c != nil || !errors.Is(err, tc.want) {
				t.Fatalf("checker %v, err %v; want none and %v", c, err, tc.want)
			}
		})
	}
}

func TestSegmentCheckerEndsAtFinal(t *testing.T) {
	eng, _ := seededEngine(t)
	a := New(eng, WithApp(testApp))
	ctx := scoped(testTenant)
	c := newChecker(ctx, t, a, presets.InfraAgentOutput)

	if _, consumed, err := c.CheckSegment(ctx, "First. Sec", false); err != nil || consumed != len("First. ") {
		t.Fatalf("first: consumed %d, err %v", consumed, err)
	}
	if decision, consumed, err := c.CheckSegment(ctx, "Second", true); err != nil || consumed != len("Second") || decision.Text != "Second" {
		t.Fatalf("final: decision %+v, consumed %d, err %v", decision, consumed, err)
	}
	if decision, consumed, err := c.CheckSegment(ctx, "late", true); !errors.Is(err, safety.ErrUnavailable) || decision.Text != "" || consumed != 0 {
		t.Fatalf("after final: decision %+v, consumed %d, err %v; want safety.ErrUnavailable", decision, consumed, err)
	}
}
