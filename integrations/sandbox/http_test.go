package sandboxbroker_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
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

func TestHTTPPreservesUncertainRuntimeObservations(t *testing.T) {
	for _, operation := range []string{"inspect-absent", "cancel-absent", "inspect-pending"} {
		t.Run(operation, func(t *testing.T) {
			previous, runtime, cfg, ctx := setup(t)
			if err := previous.Close(); err != nil {
				t.Fatal(err)
			}
			failure := errors.New("private containment failure")
			adapter := &absenceRuntime{fixtureRuntime: runtime, inspectErr: errors.Join(broker.ErrNotFound, failure), cancelErr: broker.ErrNotFound}
			if operation == "cancel-absent" {
				adapter.inspectErr = broker.ErrNotFound
				adapter.cancelErr = errors.Join(broker.ErrNotFound, failure)
			}
			if operation == "inspect-pending" {
				adapter.inspectErr = errors.Join(broker.ErrPending, failure)
			}
			cfg.Runtime = adapter
			instance, err := broker.Open(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer instance.Close()
			runtime.startAbsent, runtime.startErr = true, errors.New("lost start acknowledgement")
			task := job(t)
			if _, err = instance.Submit(ctx, task); err == nil {
				t.Fatal("ambiguous start acknowledged")
			}
			server := httptest.NewTLSServer(instance.Handler(func(*http.Request) (broker.Identity, error) { return identity(), nil }))
			defer server.Close()
			method, path := http.MethodGet, "/v1/jobs/"+task.ID.String()
			if operation == "cancel-absent" {
				method, path = http.MethodPost, path+"/cancel"
			}
			request, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusServiceUnavailable {
				t.Fatal("runtime failure became an authoritative observation", response.StatusCode)
			}
		})
	}
}
