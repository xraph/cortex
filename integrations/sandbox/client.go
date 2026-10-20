package sandboxbroker

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/sandbox"
)

// Client uses a pinned broker signing key and an authenticated HTTPS transport.
// The supplied transport owns mTLS or credential injection. Redirects are denied.
type Client struct {
	endpoint string
	http     *http.Client
	key      ed25519.PublicKey
}

// NewClient rejects non-HTTPS endpoints and copies the transport configuration.
// Set Timeout on the supplied client to bound requests; otherwise 30s is used.
func NewClient(endpoint string, transport *http.Client, key ed25519.PublicKey) (*Client, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") || transport == nil || len(key) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("cortex: invalid sandbox client configuration")
	}
	client := *transport
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if client.Timeout <= 0 {
		client.Timeout = 30 * time.Second
	}
	return &Client{endpoint: strings.TrimRight(endpoint, "/"), http: &client, key: append(ed25519.PublicKey(nil), key...)}, nil
}

func (c *Client) call(ctx context.Context, method, path string, input, output any) error {
	var body []byte
	var err error
	if input != nil {
		body, err = json.Marshal(input)
		if err != nil {
			return err
		}
		if len(body) > 1<<20 {
			return fmt.Errorf("cortex: sandbox request too large")
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusAccepted:
		return ErrPending
	case http.StatusForbidden, http.StatusUnauthorized:
		return ErrDenied
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusConflict:
		return ErrConflict
	default:
		return fmt.Errorf("cortex: sandbox broker HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 4<<20 {
		return fmt.Errorf("cortex: sandbox response too large")
	}
	return json.Unmarshal(data, output)
}

// Capabilities reads the authenticated worker attestation.
func (c *Client) Capabilities(ctx context.Context) (sandbox.Capabilities, error) {
	var result sandbox.Capabilities
	err := c.call(ctx, http.MethodGet, "/v1/capabilities", nil, &result)
	return result, err
}

// Submit checks the complete signed receipt against the requested digest.
func (c *Client) Submit(ctx context.Context, j sandbox.Job) (sandbox.Receipt, error) {
	if err := j.Validate(); err != nil {
		return sandbox.Receipt{}, err
	}
	var result sandbox.Receipt
	if err := c.call(ctx, http.MethodPost, "/v1/jobs", j, &result); err != nil {
		return sandbox.Receipt{}, err
	}
	if err := VerifyReceipt(result, c.key); err != nil {
		return sandbox.Receipt{}, err
	}
	if result.Job.Digest != j.Digest {
		return sandbox.Receipt{}, ErrConflict
	}
	return result, nil
}

// Inspect verifies receipt identity before exposing a status.
func (c *Client) Inspect(ctx context.Context, key id.ID) (sandbox.JobStatus, error) {
	var result sandbox.JobStatus
	if err := c.call(ctx, http.MethodGet, "/v1/jobs/"+key.String(), nil, &result); err != nil {
		return result, err
	}
	if err := VerifyReceipt(result.Receipt, c.key); err != nil {
		return sandbox.JobStatus{}, err
	}
	if result.Receipt.Job.ID != key {
		return sandbox.JobStatus{}, ErrConflict
	}
	return result, nil
}

// Cancel is safe to retry with the same job ID.
func (c *Client) Cancel(ctx context.Context, key id.ID) error {
	var result struct{}
	return c.call(ctx, http.MethodPost, "/v1/jobs/"+key.String()+"/cancel", nil, &result)
}

// Collect checks bounded output against the signed job before returning bytes.
func (c *Client) Collect(ctx context.Context, key id.ID) (sandbox.Result, error) {
	status, err := c.Inspect(ctx, key)
	if err != nil {
		return sandbox.Result{}, err
	}
	var result sandbox.Result
	if operationErr := c.call(ctx, http.MethodGet, "/v1/jobs/"+key.String()+"/result", nil, &result); operationErr != nil {
		return sandbox.Result{}, operationErr
	}
	if operationErr := VerifyResult(result, status.Receipt.Job, c.key); operationErr != nil {
		return sandbox.Result{}, operationErr
	}
	if result.State == sandbox.StateRunning {
		return sandbox.Result{}, ErrPending
	}
	return result, nil
}
