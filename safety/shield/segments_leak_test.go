package shield

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/xraph/cortex/safety"

	"github.com/xraph/shield/presets"
)

// streamSegments drives a fresh segment checker the way the engine does:
// it appends text in chunks of size bytes, checks the pending text, drops
// what was consumed and publishes the decision's text, then settles the
// rest with a final check. It returns everything published, and stops at
// the first check that fails, returning its error.
func streamSegments(ctx context.Context, t *testing.T, a *Adapter, profile, text string, size int) (string, error) {
	t.Helper()
	c := newChecker(ctx, t, a, profile)
	var published strings.Builder
	pending := ""
	for i := 0; i < len(text); i += size {
		pending += text[i:min(i+size, len(text))]
		decision, consumed, err := c.CheckSegment(ctx, pending, false)
		if err != nil {
			return published.String(), err
		}
		pending = pending[consumed:]
		published.WriteString(decision.Text)
	}
	decision, consumed, err := c.CheckSegment(ctx, pending, true)
	if err != nil {
		return published.String(), err
	}
	if consumed != len(pending) {
		t.Fatalf("final CheckSegment: consumed %d of %d", consumed, len(pending))
	}
	published.WriteString(decision.Text)
	return published.String(), nil
}

// requireBlockedWhole fails unless Check blocks text as one model output.
// The output profile blocks a credential rather than redacting it.
func requireBlockedWhole(ctx context.Context, t *testing.T, a *Adapter, profile, text string) {
	t.Helper()
	decision, err := a.Check(ctx, safety.PublicationRequest{
		ScanRequest: safety.ScanRequest{Content: text, Direction: safety.DirectionOutput, ProfileName: profile},
		Source:      "model_output",
	})
	if !errors.Is(err, safety.ErrBlocked) || decision.Text != "" {
		t.Fatalf("the whole message: %+v, %v; want safety.ErrBlocked", decision, err)
	}
}

// requireBlockedBefore fails unless the stream was blocked and published
// only text that comes before secret, so no piece of it went out.
func requireBlockedBefore(t *testing.T, published string, err error, text, secret string) {
	t.Helper()
	if !errors.Is(err, safety.ErrBlocked) {
		t.Fatalf("published %q, err %v; want safety.ErrBlocked", published, err)
	}
	at := strings.Index(text, secret)
	if at < 0 {
		t.Fatalf("%q is not in the text", secret)
	}
	if !strings.HasPrefix(text, published) || len(published) > at {
		t.Fatalf("published %q, past the start of %q", published, secret)
	}
}

func TestSegmentKeepsAssignmentsWhole(t *testing.T) {
	eng, _ := seededEngine(t)
	a := New(eng, WithApp(testApp))
	ctx := scoped(testTenant)
	cases := []struct{ text, value string }{
		{"password: Hunter2-prod-Pass\n", "Hunter2-prod-Pass"},
		{"The api_key: qz81mXv02LpTa\n", "qz81mXv02LpTa"},
	}
	for _, tc := range cases {
		requireBlockedWhole(ctx, t, a, presets.InfraAgentOutput, tc.text)
		for _, size := range []int{1, 3, 4} {
			t.Run(fmt.Sprintf("%q/%d", tc.text, size), func(t *testing.T) {
				got, err := streamSegments(ctx, t, a, presets.InfraAgentOutput, tc.text, size)
				requireBlockedBefore(t, got, err, tc.text, tc.value)
			})
		}
	}
}

func TestSegmentLimitCutKeepsAssignmentsWhole(t *testing.T) {
	eng, _ := seededEngine(t)
	a := New(eng, WithApp(testApp), WithSegmentLimit(64))
	ctx := scoped(testTenant)
	// A run over the limit with no sentence or line end, whose last
	// whitespace sits inside an assignment.
	for _, prefix := range []string{"password: Hun", "password = Hun", "password =Hun", "password:\tHun", "password ", "password\t ", `"password" `} {
		pending := strings.Repeat("words go here ", 5) + prefix
		decision, consumed, err := newChecker(ctx, t, a, presets.InfraAgentOutput).CheckSegment(ctx, pending, false)
		if err != nil {
			t.Fatalf("CheckSegment: %v", err)
		}
		if want := len(pending) - len(prefix); consumed != want || strings.Contains(decision.Text, "password") {
			t.Fatalf("%q: consumed %d, text %q; want %d, cut before the key", prefix, consumed, decision.Text, want)
		}
	}
}

func TestSegmentLimitCutWaitsForTheOperator(t *testing.T) {
	eng, _ := seededEngine(t)
	const limit = 64
	a := New(eng, WithApp(testApp), WithSegmentLimit(limit))
	ctx := scoped(testTenant)
	pairs := []struct{ pair, value string }{
		{"password = Hunter2-prod-Pass", "Hunter2-prod-Pass"},
		{"DB_PASSWORD =Hunter2-prod-Pass", "Hunter2-prod-Pass"},
		{`"password" : "Hunter2-prod-Pass"`, "Hunter2-prod-Pass"},
	}
	// A line with no sentence or line end before the pair, padded so the
	// limit is crossed at every byte around the key's blank.
	words := strings.Repeat("words go here ", 10)
	for _, p := range pairs {
		for pad := limit - 24; pad <= limit+4; pad++ {
			text := words[:pad] + " " + p.pair + "\n"
			requireBlockedWhole(ctx, t, a, presets.InfraAgentOutput, text)
			for size := 1; size <= 4; size++ {
				t.Run(fmt.Sprintf("%s/%d/%d", p.pair, pad, size), func(t *testing.T) {
					got, err := streamSegments(ctx, t, a, presets.InfraAgentOutput, text, size)
					requireBlockedBefore(t, got, err, text, p.value)
				})
			}
		}
	}
}

// privateKeyPEM returns a freshly generated private key of kind as PEM.
func privateKeyPEM(t *testing.T, kind string) string {
	t.Helper()
	var block *pem.Block
	switch kind {
	case "rsa-pkcs8", "rsa-pkcs1":
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		if kind == "rsa-pkcs1" {
			block = &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
			break
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		block = &pem.Block{Type: "PRIVATE KEY", Bytes: der}
	case "ec":
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		der, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		block = &pem.Block{Type: "EC PRIVATE KEY", Bytes: der}
	default:
		t.Fatalf("unknown key kind %q", kind)
	}
	return string(pem.EncodeToMemory(block))
}

func TestSegmentHoldsPrivateKeysWhole(t *testing.T) {
	eng, _ := seededEngine(t)
	a := New(eng, WithApp(testApp))
	ctx := scoped(testTenant)
	for _, kind := range []string{"rsa-pkcs8", "rsa-pkcs1", "ec"} {
		for _, size := range []int{1, 3, 4} {
			runs := 1
			if size == 4 {
				runs = 30
			}
			t.Run(fmt.Sprintf("%s/%d", kind, size), func(t *testing.T) {
				for range runs {
					key := privateKeyPEM(t, kind)
					text := "Here is the key you asked for.\n" + key + "Keep it out of the repo.\n"
					requireBlockedWhole(ctx, t, a, presets.InfraAgentOutput, text)
					got, err := streamSegments(ctx, t, a, presets.InfraAgentOutput, text, size)
					requireBlockedBefore(t, got, err, text, key)
				}
			})
		}
	}
}

func TestSegmentHoldsFromAnUnclosedBegin(t *testing.T) {
	eng, _ := seededEngine(t)
	a := New(eng, WithApp(testApp))
	ctx := scoped(testTenant)
	cases := []struct{ pending, want string }{
		// Held from the BEGIN line until its END line arrives.
		{"Key follows.\n-----BEGIN EC PRIVATE KEY-----\nMHcCAQEEI\n", "Key follows.\n"},
		{"Key: -----BEGIN EC PRIVATE KEY-----\nMHcCAQEEI\n", ""},
		// A closed block can go, as one segment with the text around it.
		{"-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\nNext li", "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"},
		// An END for another label does not close the block.
		{"-----BEGIN EC PRIVATE KEY-----\nMHc\n-----END CERTIFICATE-----\nmore\n", ""},
		// A closed block followed by an unclosed one is held from the second.
		{"-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\nthen\n-----BEGIN PRIVATE KEY-----\nMIIE\n", "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\nthen\n"},
	}
	for _, tc := range cases {
		decision, consumed, err := newChecker(ctx, t, a, presets.InfraAgentOutput).CheckSegment(ctx, tc.pending, false)
		if err != nil {
			t.Fatalf("%q: %v", tc.pending, err)
		}
		if consumed != len(tc.want) || (tc.want == "" && decision.Text != "") {
			t.Fatalf("%q: consumed %d, text %q; want %d", tc.pending, consumed, decision.Text, len(tc.want))
		}
	}
}
