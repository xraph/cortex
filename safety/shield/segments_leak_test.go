package shield

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"strings"
	"testing"

	"github.com/xraph/cortex/safety"

	"github.com/xraph/shield/presets"
)

// streamSegments drives a fresh segment checker the way the engine does:
// it appends text in chunks of size bytes, checks the pending text, drops
// what was consumed and publishes the decision's text, then settles the
// rest with a final check. It returns everything published.
func streamSegments(ctx context.Context, t *testing.T, a *Adapter, profile, text string, size int) string {
	t.Helper()
	c := newChecker(ctx, t, a, profile)
	var published strings.Builder
	pending := ""
	for i := 0; i < len(text); i += size {
		pending += text[i:min(i+size, len(text))]
		decision, consumed, err := c.CheckSegment(ctx, pending, false)
		if err != nil {
			t.Fatalf("CheckSegment: %v", err)
		}
		pending = pending[consumed:]
		published.WriteString(decision.Text)
	}
	decision, consumed, err := c.CheckSegment(ctx, pending, true)
	if err != nil || consumed != len(pending) {
		t.Fatalf("final CheckSegment: consumed %d of %d, err %v", consumed, len(pending), err)
	}
	published.WriteString(decision.Text)
	return published.String()
}

// wholeMessage returns what Check publishes for text as one model output.
func wholeMessage(ctx context.Context, t *testing.T, a *Adapter, profile, text string) string {
	t.Helper()
	decision, err := a.Check(ctx, safety.PublicationRequest{
		ScanRequest: safety.ScanRequest{Content: text, Direction: safety.DirectionOutput, ProfileName: profile},
		Source:      "model_output",
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return decision.Text
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
		whole := wholeMessage(ctx, t, a, presets.InfraAgentOutput, tc.text)
		if strings.Contains(whole, tc.value) {
			t.Fatalf("the whole message kept %q: %q", tc.value, whole)
		}
		for _, size := range []int{1, 3, 4} {
			t.Run(fmt.Sprintf("%q/%d", tc.text, size), func(t *testing.T) {
				got := streamSegments(ctx, t, a, presets.InfraAgentOutput, tc.text, size)
				if got != whole || strings.Contains(got, tc.value) {
					t.Fatalf("streamed %q, the whole message publishes %q", got, whole)
				}
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
	for _, prefix := range []string{"password: Hun", "password = Hun", "password =Hun", "password:\tHun"} {
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

// requireNoKeyLine fails when published holds any line of key.
func requireNoKeyLine(t *testing.T, published, key string) {
	t.Helper()
	for _, line := range strings.Split(key, "\n") {
		if line != "" && strings.Contains(published, line) {
			t.Fatalf("published a line of the key: %q", line)
		}
	}
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
					whole := wholeMessage(ctx, t, a, presets.InfraAgentOutput, text)
					requireNoKeyLine(t, whole, key)
					got := streamSegments(ctx, t, a, presets.InfraAgentOutput, text, size)
					requireNoKeyLine(t, got, key)
					if got != whole {
						t.Fatalf("streamed %q, the whole message publishes %q", got, whole)
					}
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
