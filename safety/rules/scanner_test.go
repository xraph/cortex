package rules_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/safety"
	"github.com/xraph/cortex/safety/rules"
)

func TestScannerBoundsAndDetections(t *testing.T) {
	cfg := rules.Config{Revision: "host-policy-1", MaxBytes: 128, Rules: []rules.Rule{
		{ID: "secret", Pattern: `(?i)secret_[a-z0-9]{8,}`, Decision: safety.DecisionBlock},
		{ID: "email", Pattern: `[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`, Decision: safety.DecisionRedact, Replacement: "[email]"},
	}}
	scanner, err := rules.New(cfg)
	require.NoError(t, err)
	cfg.Rules[0].Pattern = "never matches" // construction freezes the policy
	for _, tc := range []struct {
		input    string
		decision safety.Decision
		text     string
	}{{"ordinary output", safety.DecisionAllow, ""}, {"secret_12345678", safety.DecisionBlock, ""}, {"mail a@example.com", safety.DecisionRedact, "mail [email]"}, {"a@example.com secret_12345678", safety.DecisionBlock, ""}} {
		for _, direction := range []safety.Direction{safety.DirectionInput, safety.DirectionOutput} {
			req := &safety.ScanRequest{Content: tc.input, Direction: direction}
			var result *safety.ScanResult
			if direction == safety.DirectionInput {
				result, err = scanner.ScanInput(context.Background(), req)
			} else {
				result, err = scanner.ScanOutput(context.Background(), req)
			}
			require.NoError(t, err)
			require.Equal(t, tc.decision, result.Decision)
			require.Equal(t, tc.text, result.Redacted)
			require.Equal(t, "host-policy-1", result.ProfileUsed)
			for _, finding := range result.Findings {
				require.NotContains(t, finding.Message, tc.input)
			}
		}
	}
	_, err = scanner.ScanInput(context.Background(), &safety.ScanRequest{Content: string(make([]byte, 129))})
	require.ErrorIs(t, err, safety.ErrLimit)
	_, err = scanner.ScanOutput(context.Background(), &safety.ScanRequest{Content: string([]byte{0xff})})
	require.ErrorIs(t, err, safety.ErrUnavailable)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = scanner.ScanOutput(ctx, &safety.ScanRequest{Content: "safe"})
	require.ErrorIs(t, err, context.Canceled)
	_, err = scanner.ScanOutput(context.Background(), nil)
	require.ErrorIs(t, err, safety.ErrUnavailable)
}

func TestScannerRejectsEmptyInvalidAndAmbiguousPolicy(t *testing.T) {
	for _, cfg := range []rules.Config{{}, {Revision: "1", MaxBytes: 100}, {Revision: "1", MaxBytes: 100, Rules: []rules.Rule{{ID: "bad", Pattern: "[", Decision: safety.DecisionBlock}}}, {Revision: "1", MaxBytes: 100, Rules: []rules.Rule{{ID: "empty", Pattern: "a*", Decision: safety.DecisionBlock}}}, {Revision: "1", MaxBytes: 100, Rules: []rules.Rule{{ID: "allow", Pattern: "x", Decision: safety.DecisionAllow}}}, {Revision: "1", MaxBytes: 100, Rules: []rules.Rule{{ID: "x", Pattern: "x", Decision: safety.DecisionBlock}, {ID: "x", Pattern: "y", Decision: safety.DecisionBlock}}}} {
		_, err := rules.New(cfg)
		require.Error(t, err)
	}
}

func TestScannerBlocksPatternsIntroducedByLaterRedaction(t *testing.T) {
	scanner, err := rules.New(rules.Config{Revision: "1", MaxBytes: 128, Rules: []rules.Rule{
		{ID: "email", Pattern: `[a-z]+@[a-z]+\.[a-z]+`, Decision: safety.DecisionRedact, Replacement: "[email]"},
		{ID: "space", Pattern: ` +`, Decision: safety.DecisionRedact, Replacement: ""},
	}})
	require.NoError(t, err)
	result, err := scanner.ScanOutput(context.Background(), &safety.ScanRequest{Content: "a@example .com"})
	require.NoError(t, err)
	require.True(t, result.Blocked)
	require.Empty(t, result.Redacted)
	for _, pattern := range []string{`\b`, `^`, `x?`, `a|\b`} {
		_, err = rules.New(rules.Config{Revision: "1", MaxBytes: 128, Rules: []rules.Rule{{ID: "zero", Pattern: pattern, Decision: safety.DecisionRedact}}})
		require.Error(t, err)
	}
	_, err = new(rules.Scanner).ScanOutput(context.Background(), &safety.ScanRequest{})
	require.ErrorIs(t, err, safety.ErrUnavailable)
}
