//go:build linux || darwin

package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/image"
	"github.com/xraph/cortex/integrations/sandbox/internal/verifiedio"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerpeer"
	"github.com/xraph/cortex/sandbox"
)

// ImagePolicy checks current source access for the signed job's manifest pin.
// The job and context scope are detached from retained controller state.
type ImagePolicy func(context.Context, sandbox.Job, string) error

// WorkerImages serves only the signed manifest and its directly declared blobs.
// Registry credentials stay in the daemon; workers cannot supply URLs or tags.
type WorkerImages struct {
	controller *Controller
	source     image.Source
	policy     ImagePolicy
	maximum    int64
}

// NewWorkerImages requires an approved content source, current source policy and
// aggregate compressed-image ceiling. It does not qualify or execute the image.
func NewWorkerImages(controller *Controller, source image.Source, policy ImagePolicy, maximum int64) (*WorkerImages, error) {
	if controller == nil || source == nil || policy == nil || maximum <= 0 || maximum > 4<<30 {
		return nil, fmt.Errorf("cortex: invalid worker image services")
	}
	return &WorkerImages{controller: controller, source: source, policy: policy, maximum: maximum}, nil
}

// Read copies one approved blob and returns its exact size only on success.
// Bytes remain provisional until all content, close and authority checks pass.
// The caller must unblock destination writes on cancellation.
func (s *WorkerImages) Read(ctx context.Context, peer *workerpeer.Peer, claim WorkerClaim, pin string, destination io.Writer) (int64, error) {
	if peer == nil {
		return 0, broker.ErrDenied
	}
	return s.read(ctx, peer, claim, pin, destination)
}

func (s *WorkerImages) authorize(ctx context.Context, peer workerIdentity, claim WorkerClaim) (sandbox.Job, error) {
	current, err := s.controller.authorizeWorker(ctx, peer, claim)
	if err != nil {
		return sandbox.Job{}, err
	}
	job := current.Receipt.Job
	candidate, err := sandbox.Seal(job)
	if err != nil {
		return sandbox.Job{}, err
	}
	if policyErr := s.policy(evidenceContext(ctx, job), candidate, job.Image); policyErr != nil {
		return sandbox.Job{}, policyErr
	}
	current, err = s.controller.authorizeWorker(ctx, peer, claim)
	if err != nil {
		return sandbox.Job{}, err
	}
	return current.Receipt.Job, ctx.Err()
}

func (s *WorkerImages) read(ctx context.Context, peer workerIdentity, claim WorkerClaim, pin string, destination io.Writer) (int64, error) {
	if destination == nil || !image.ValidDigest(pin) {
		return 0, broker.ErrDenied
	}
	operation, finish, err := s.controller.beginWorkerOperation(ctx, peer, claim)
	if err != nil {
		return 0, err
	}
	defer finish()
	job, err := s.authorize(operation, peer, claim)
	if err != nil {
		return 0, err
	}
	var metadata bytes.Buffer
	if copyErr := s.copy(operation, peer, claim, job.Image, -1, min(s.maximum, 1<<20), &metadata); copyErr != nil {
		return 0, copyErr
	}
	manifest, err := image.ParseManifest(metadata.Bytes(), s.maximum)
	if err != nil {
		return 0, err
	}
	var size int64
	if pin == job.Image {
		n, writeErr := destination.Write(metadata.Bytes())
		if writeErr != nil {
			return 0, writeErr
		}
		if n != metadata.Len() {
			return 0, io.ErrShortWrite
		}
		size = int64(n)
	} else {
		size = -1
		if manifest.Config.Digest.String() == pin {
			size = manifest.Config.Size
		}
		for _, layer := range manifest.Layers {
			if layer.Digest.String() == pin {
				size = layer.Size
				break
			}
		}
		if size < 0 {
			return 0, broker.ErrDenied
		}
		if copyErr := s.copy(operation, peer, claim, pin, size, size, destination); copyErr != nil {
			return 0, copyErr
		}
	}
	if _, authorizationErr := s.authorize(operation, peer, claim); authorizationErr != nil {
		return 0, authorizationErr
	}
	if operationErr := operation.Err(); operationErr != nil {
		return 0, operationErr
	}
	return size, nil
}

func (s *WorkerImages) copy(ctx context.Context, peer workerIdentity, claim WorkerClaim, pin string, size, maximum int64, destination io.Writer) error {
	job, err := s.authorize(ctx, peer, claim)
	if err != nil {
		return err
	}
	reader, err := s.source.Open(evidenceContext(ctx, job), pin)
	if err != nil {
		if reader != nil {
			err = errors.Join(err, reader.Close())
		}
		return err
	}
	if reader == nil {
		return fmt.Errorf("cortex: image source returned no reader")
	}
	if _, authorizationErr := s.authorize(ctx, peer, claim); authorizationErr != nil {
		return errors.Join(authorizationErr, reader.Close())
	}
	if copyErr := verifiedio.Read(ctx, reader, destination, size, maximum, pin[7:]); copyErr != nil {
		return copyErr
	}
	_, err = s.authorize(ctx, peer, claim)
	return errors.Join(err, ctx.Err())
}
