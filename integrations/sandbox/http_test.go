package sandboxbroker_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"testing"

	broker "github.com/xraph/cortex/integrations/sandbox"
)

func TestAuthenticatedClientRoundTrip(t *testing.T) {
	b, _, _, _ := setup(t)
	server := httptest.NewTLSServer(b.Handler(func(*http.Request) (broker.Identity, error) { return identity(), nil }))
	defer server.Close()
	client, err := broker.NewClient(server.URL, server.Client(), b.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	j := job(t)
	if _, err = client.Submit(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	if err = client.Cancel(context.Background(), j.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = client.Collect(context.Background(), j.ID); err != nil {
		t.Fatal(err)
	}
}
func TestHTTPRejectsUnknownPrivilegeFields(t *testing.T) {
	b, _, _, _ := setup(t)
	server := httptest.NewTLSServer(b.Handler(func(*http.Request) (broker.Identity, error) { return identity(), nil }))
	defer server.Close()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL+"/v1/jobs", bytes.NewBufferString(`{"privileged":true}`))
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatal(response.StatusCode)
	}
}
func TestReceiptTamperingRejected(t *testing.T) {
	b, _, _, ctx := setup(t)
	receipt, err := b.Submit(ctx, job(t))
	if err != nil {
		t.Fatal(err)
	}
	receipt.Capabilities.Profile = "host"
	if err = broker.VerifyReceipt(receipt, b.PublicKey()); err == nil {
		t.Fatal("tampered receipt accepted")
	}
	if _, err = broker.NewClient("http://localhost", http.DefaultClient, make(ed25519.PublicKey, ed25519.PublicKeySize)); err == nil {
		t.Fatal("plaintext transport accepted")
	}
}
