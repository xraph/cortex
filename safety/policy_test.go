package safety

import (
	"context"
	"errors"
	"testing"
)

type fixedScanner struct {
	result *ScanResult
	err    error
}

func (s fixedScanner) ScanInput(context.Context, *ScanRequest) (*ScanResult, error) {
	return s.result, s.err
}
func (s fixedScanner) ScanOutput(context.Context, *ScanRequest) (*ScanResult, error) {
	return s.result, s.err
}

func TestPublicationDecisionIsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result *ScanResult
		err    error
		want   string
		fail   bool
	}{
		{name: "allow", result: &ScanResult{Decision: DecisionAllow}, want: "private"},
		{name: "flag", result: &ScanResult{Decision: DecisionFlag}, want: "private"},
		{name: "legacy allow redaction", result: &ScanResult{Decision: DecisionAllow, Redacted: "safe"}, want: "safe"},
		{name: "redact", result: &ScanResult{Decision: DecisionRedact, Redacted: "safe"}, want: "safe"},
		{name: "empty redaction", result: &ScanResult{Decision: DecisionRedact}},
		{name: "block", result: &ScanResult{Decision: DecisionBlock}, fail: true},
		{name: "conflicting block", result: &ScanResult{Decision: DecisionAllow, Blocked: true}, fail: true},
		{name: "unknown", result: &ScanResult{Decision: "future"}, fail: true},
		{name: "nil", fail: true}, {name: "error", err: errors.New("private"), fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := ScannerGate{Scanner: fixedScanner{tc.result, tc.err}}
			got, err := gate.Check(context.Background(), PublicationRequest{ScanRequest: ScanRequest{Content: "private", Direction: DirectionOutput}})
			if (err != nil) != tc.fail {
				t.Fatalf("decision=%+v err=%v", got, err)
			}
			if tc.fail && got.Text != "" {
				t.Fatal("failed check returned raw text")
			}
			if !tc.fail && got.Text != tc.want {
				t.Fatalf("got %q want %q", got.Text, tc.want)
			}
		})
	}
}

func TestPublicationPolicyValidation(t *testing.T) {
	for _, p := range []Policy{{Mode: "future"}, {Mode: ModeDisabled, Required: true}, {MaxOutputBytes: -1}} {
		if _, err := p.Resolve(); err == nil {
			t.Fatalf("invalid policy accepted: %+v", p)
		}
	}
	p, err := (Policy{}).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode != ModeBuffered || p.MaxInputBytes <= 0 || p.MaxOutputBytes <= 0 || p.MaxToolOutputBytes <= 0 {
		t.Fatal("defaults must be bounded and buffered")
	}
}
