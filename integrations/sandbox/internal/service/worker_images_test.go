package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	oci "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/sandbox"
)

type workerImageSource struct {
	blobs         map[string][]byte
	opens, closes int
	open          func(context.Context, string) (io.ReadCloser, error)
}

func (s *workerImageSource) Open(ctx context.Context, pin string) (io.ReadCloser, error) {
	s.opens++
	if s.open != nil {
		return s.open(ctx, pin)
	}
	body, ok := s.blobs[pin]
	if !ok {
		return nil, evidence.ErrNotFound
	}
	return &workerEvidenceReader{Reader: bytes.NewReader(body), closed: func() { s.closes++ }}, ctx.Err()
}
func imagePin(body []byte) string { return "sha256:" + evidence.Digest(body) }
func imageAccessFixture(t *testing.T, changes ...func(*oci.Manifest)) (*WorkerImages, *Controller, WorkerClaim, *fixtureWorkerPeer, *workerImageSource, string, string) {
	t.Helper()
	config, layer := []byte(`{"architecture":"arm64"}`), []byte("approved layer")
	manifest := oci.Manifest{MediaType: oci.MediaTypeImageManifest, Config: oci.Descriptor{MediaType: oci.MediaTypeImageConfig, Digest: digest.Digest(imagePin(config)), Size: int64(len(config))}, Layers: []oci.Descriptor{{MediaType: oci.MediaTypeImageLayer, Digest: digest.Digest(imagePin(layer)), Size: int64(len(layer))}}}
	manifest.SchemaVersion = 2
	for _, change := range changes {
		change(&manifest)
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	root := imagePin(body)
	c, receipt, _ := controllerFixture(t, func(job *sandbox.Job) { job.Image = root })
	c, claim, peer, _ := authorizationControllerFixture(t, c, receipt)
	source := &workerImageSource{blobs: map[string][]byte{root: body, imagePin(config): config, imagePin(layer): layer}}
	access, err := NewWorkerImages(c, source, func(ctx context.Context, job sandbox.Job, pin string) error {
		if pin != root || job.Image != root || job.Principal != receipt.Job.Principal || cortex.ScopeFromContext(ctx).Canonical() != job.Scope.Canonical() {
			t.Error("image policy lost signed identity")
			return broker.ErrDenied
		}
		job.Scope.Levels[0].Value = "changed"
		return ctx.Err()
	}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return access, c, claim, peer, source, root, imagePin(layer)
}

func TestWorkerImagesServeOnlyPinnedManifestMembers(t *testing.T) {
	access, _, claim, peer, source, root, layer := imageAccessFixture(t)
	pins := []string{root, layer}
	for pin := range source.blobs {
		if pin != root && pin != layer {
			pins = append(pins, pin)
		}
	}
	for _, pin := range pins {
		var got bytes.Buffer
		size, err := access.read(t.Context(), peer, claim, pin, &got)
		if err != nil || size != int64(len(source.blobs[pin])) || !bytes.Equal(got.Bytes(), source.blobs[pin]) {
			t.Fatal("image member transfer", size, err)
		}
	}
	before := source.opens
	var got bytes.Buffer
	if _, err := access.read(t.Context(), peer, claim, imagePin([]byte("unrelated")), &got); err == nil || got.Len() != 0 || source.opens != before+1 {
		t.Fatal("unlisted blob accessed", err)
	}
	if source.closes != source.opens {
		t.Fatal("image reader leaked")
	}
}

func TestWorkerImagesRejectInvalidAuthorityBeforeProviderAccess(t *testing.T) {
	for _, mode := range []string{"peer", "claim", "policy", "invalid-pin", "nil-destination"} {
		t.Run(mode, func(t *testing.T) {
			access, _, claim, peer, source, _, pin := imageAccessFixture(t)
			destination := io.Discard
			switch mode {
			case "peer":
				peer.dieAfterFirst.Store(true)
			case "claim":
				claim.Revision++
			case "policy":
				access.policy = func(context.Context, sandbox.Job, string) error { return broker.ErrDenied }
			case "invalid-pin":
				pin = "https://external.invalid/blob"
			case "nil-destination":
				destination = nil
			}
			if _, err := access.read(t.Context(), peer, claim, pin, destination); err == nil || source.opens != 0 {
				t.Fatal("untrusted request reached image provider", err)
			}
		})
	}
}

func TestWorkerImagesRejectCorruptAndUncertainSources(t *testing.T) {
	for _, mode := range []string{"manifest-digest", "layer-digest", "partial-layer", "oversized-layer", "nil-reader", "close-error", "open-error-reader", "revoked-open", "revoked-read"} {
		t.Run(mode, func(t *testing.T) {
			access, _, claim, peer, source, root, pin := imageAccessFixture(t)
			var revoked atomic.Bool
			policy := access.policy
			access.policy = func(ctx context.Context, job sandbox.Job, pin string) error {
				if revoked.Load() {
					return broker.ErrDenied
				}
				return policy(ctx, job, pin)
			}
			switch mode {
			case "manifest-digest":
				source.blobs[root] = []byte("changed")
			case "layer-digest":
				source.blobs[pin] = []byte("corrupt layer!")
			case "partial-layer":
				source.blobs[pin] = []byte("short")
			case "oversized-layer":
				source.blobs[pin] = append(source.blobs[pin], 'x')
			default:
				source.open = func(_ context.Context, key string) (io.ReadCloser, error) {
					if key == root {
						return io.NopCloser(bytes.NewReader(source.blobs[root])), nil
					}
					switch mode {
					case "nil-reader":
						return nil, nil
					case "close-error":
						return closingEvidenceReader{Reader: bytes.NewReader(source.blobs[pin])}, nil
					case "open-error-reader":
						return &workerEvidenceReader{Reader: bytes.NewReader(nil), closed: func() { source.closes++ }}, errors.New("source failed")
					case "revoked-open":
						revoked.Store(true)
					case "revoked-read":
						return &workerEvidenceReader{Reader: bytes.NewReader(source.blobs[pin]), closed: func() { revoked.Store(true) }}, nil
					}
					return io.NopCloser(bytes.NewReader(source.blobs[pin])), nil
				}
			}
			var got bytes.Buffer
			if size, err := access.read(t.Context(), peer, claim, pin, &got); err == nil || size != 0 {
				t.Fatal("unverified image transfer acknowledged", size, err)
			}
			if mode == "revoked-open" && got.Len() != 0 {
				t.Fatal("bytes copied after revocation")
			}
			if mode == "open-error-reader" && source.closes != 1 {
				t.Fatal("reader returned with error leaked")
			}
		})
	}
}

func TestWorkerImagesCancellationClosesSourceAndJoins(t *testing.T) {
	for _, member := range []string{"manifest", "layer"} {
		t.Run(member, func(t *testing.T) {
			access, _, claim, peer, provider, root, pin := imageAccessFixture(t)
			blocked := &blockingEvidenceReader{entered: make(chan struct{}), closed: make(chan struct{})}
			provider.open = func(_ context.Context, key string) (io.ReadCloser, error) {
				if member == "manifest" || key == pin {
					return blocked, nil
				}
				return io.NopCloser(bytes.NewReader(provider.blobs[root])), nil
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := access.read(ctx, peer, claim, pin, io.Discard); done <- err }()
			select {
			case <-blocked.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("source did not start")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("source did not join")
			}
			if blocked.closes.Load() != 1 {
				t.Fatal("reader close count", blocked.closes.Load())
			}
		})
	}
}

func TestWorkerImagesValidateManifestBeforeReturningContent(t *testing.T) {
	for _, mode := range []string{"external-url", "index", "budget"} {
		t.Run(mode, func(t *testing.T) {
			access, _, claim, peer, source, root, _ := imageAccessFixture(t, func(m *oci.Manifest) {
				if mode == "external-url" {
					m.Config.URLs = []string{"https://external.invalid/config"}
				}
				if mode == "index" {
					m.MediaType = oci.MediaTypeImageIndex
				}
			})
			if mode == "budget" {
				access.maximum = int64(len(source.blobs[root]))
			}
			var got bytes.Buffer
			if size, err := access.read(t.Context(), peer, claim, root, &got); err == nil || size != 0 || got.Len() != 0 || source.opens != 1 {
				t.Fatal("invalid manifest escaped broker", size, err)
			}
		})
	}
}

func TestWorkerImagesPublicBoundaryAndConfiguration(t *testing.T) {
	access, c, claim, _, source, root, _ := imageAccessFixture(t)
	if _, err := access.Read(t.Context(), nil, claim, root, io.Discard); !errors.Is(err, broker.ErrDenied) || source.opens != 0 {
		t.Fatal("missing kernel peer", err)
	}
	for _, maximum := range []int64{0, -1, (4 << 30) + 1} {
		if service, err := NewWorkerImages(c, source, access.policy, maximum); err == nil || service != nil {
			t.Fatal("invalid budget accepted")
		}
	}
	if service, err := NewWorkerImages(nil, source, access.policy, 1024); err == nil || service != nil {
		t.Fatal("missing controller")
	}
	if service, err := NewWorkerImages(c, nil, access.policy, 1024); err == nil || service != nil {
		t.Fatal("missing source")
	}
	if service, err := NewWorkerImages(c, source, nil, 1024); err == nil || service != nil {
		t.Fatal("missing policy")
	}
}

type shortImageDestination struct{}

func (shortImageDestination) Write(body []byte) (int, error) { return len(body) - 1, nil }
func TestWorkerImagesPreserveDestinationFailures(t *testing.T) {
	for _, manifest := range []bool{false, true} {
		access, _, claim, peer, source, root, pin := imageAccessFixture(t)
		if manifest {
			pin = root
		}
		size, err := access.read(t.Context(), peer, claim, pin, shortImageDestination{})
		if !errors.Is(err, io.ErrShortWrite) || size != 0 || source.closes != source.opens {
			t.Fatal("short destination acknowledged or reader leaked", size, err)
		}
	}
}
