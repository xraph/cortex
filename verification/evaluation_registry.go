package verification

import (
	"context"
	"encoding/json"
	"time"

	"github.com/xraph/cortex"
)

type evaluationKey struct{ binding, criterion, revision string }

// EvaluationRegistry holds host-registered manifests for captured executions.
// It is not an authorization service or a durable registration store. Registered
// manifests are immutable; changes require a new registry. Reads are concurrent.
type EvaluationRegistry struct{ manifests map[evaluationKey][]byte }

// NewEvaluationRegistry freezes up to 256 already sealed manifests, with a
// combined encoding limit of sixteen MiB. It rejects ambiguous registrations.
func NewEvaluationRegistry(manifests []EvaluationManifest) (*EvaluationRegistry, error) {
	if len(manifests) == 0 || len(manifests) > 256 {
		return nil, ErrInvalid
	}
	r := &EvaluationRegistry{manifests: make(map[evaluationKey][]byte, len(manifests))}
	remaining := 16 << 20
	for _, m := range manifests {
		if m.Validate() != nil {
			return nil, ErrInvalid
		}
		key, err := evaluationRequestKey(TestRequest{CheckRequest: CheckRequest{Binding: m.Binding, Criterion: m.Criterion}})
		if err != nil {
			return nil, err
		}
		if _, exists := r.manifests[key]; exists {
			return nil, ErrInvalid
		}
		body, err := json.Marshal(m)
		if err != nil || len(body) > remaining {
			return nil, ErrInvalid
		}
		remaining -= len(body)
		r.manifests[key] = body
	}
	return r, nil
}

// Resolve selects an exact registered execution/check in the context's scope.
// It returns a detached copy and checks evidence timing. A caller must still
// authorize current access, qualify the runner and supervise grading externally.
func (r *EvaluationRegistry) Resolve(ctx context.Context, req TestRequest, now time.Time) (EvaluationManifest, error) {
	if err := ctx.Err(); err != nil {
		return EvaluationManifest{}, err
	}
	if r == nil {
		return EvaluationManifest{}, ErrInvalid
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return EvaluationManifest{}, err
	}
	if scope.Canonical() != req.Binding.Scope.Canonical() {
		return EvaluationManifest{}, ErrInvalid
	}
	key, err := evaluationRequestKey(req)
	if err != nil {
		return EvaluationManifest{}, err
	}
	body, ok := r.manifests[key]
	if !ok {
		return EvaluationManifest{}, ErrNotFound
	}
	var m EvaluationManifest
	if json.Unmarshal(body, &m) != nil || m.ValidateRequest(req, now) != nil {
		return EvaluationManifest{}, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return EvaluationManifest{}, err
	}
	return m, nil
}

func evaluationRequestKey(req TestRequest) (evaluationKey, error) {
	if req.Criterion.validate() != nil || req.Criterion.Kind != KindTest {
		return evaluationKey{}, ErrInvalid
	}
	digest, err := req.Binding.Digest()
	if err != nil {
		return evaluationKey{}, err
	}
	return evaluationKey{binding: digest, criterion: req.Criterion.ID, revision: req.Criterion.Parameters.ManifestRevision}, nil
}
