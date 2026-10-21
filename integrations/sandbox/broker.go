package sandboxbroker

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/sandbox/internal/journal"
	"github.com/xraph/cortex/sandbox"
)

// Runtime owns durable idempotency for Start, including completed-job tombstones.
// A lost response must never cause the same ID to execute twice. Cancel must stop
// the whole workload and retain its terminal status. No host execution fallback
// is permitted. Start receives the signed original acceptance, including its
// deadline and runtime binding. Verify it against the configured broker key;
// retries must never reset AcceptedAt. Qualification belongs to the adapter.
type Runtime interface {
	Capabilities(context.Context) (sandbox.Capabilities, error)
	Start(context.Context, sandbox.Receipt) error
	Inspect(context.Context, id.ID) (sandbox.Result, error)
	Cancel(context.Context, id.ID) error
}

// Config is host configuration. Images are immutable allowlisted digests.
type Config struct {
	Database   string
	Name       string
	SigningKey ed25519.PrivateKey
	Runtime    Runtime
	Authorize  Authorizer
	MaxJobs    int
	MaxActive  int
	Images     []string
	Now        func() time.Time // Trusted host clock; defaults to time.Now.
}

// Broker serializes journal transitions and authenticates every access. A single
// process holds the database's exclusive lock; concurrent opens fail closed.
type Broker struct {
	mu     sync.Mutex
	db     *sql.DB
	conn   *sql.Conn
	cfg    Config
	images map[string]bool
}

type record struct {
	Status     sandbox.JobStatus `json:"status"`
	Result     *sandbox.Result   `json:"result,omitempty"`
	Cancelling bool              `json:"cancelling"`
}

// Open acquires the durable journal before accepting work. Database must be a
// host-owned local path. Protect it and the runtime's state with host permissions.
func Open(cfg Config) (*Broker, error) {
	if cfg.Database == "" || cfg.Database == ":memory:" || cfg.Name == "" || len(cfg.Name) > 512 || len(cfg.SigningKey) != ed25519.PrivateKeySize || cfg.Runtime == nil || cfg.Authorize == nil || len(cfg.Images) == 0 {
		return nil, fmt.Errorf("cortex: incomplete sandbox broker configuration")
	}
	if !filepath.IsAbs(cfg.Database) || filepath.Clean(cfg.Database) != cfg.Database || strings.Contains(cfg.Database, "?") {
		return nil, fmt.Errorf("cortex: sandbox journal requires an absolute file path")
	}
	if cfg.MaxJobs <= 0 {
		cfg.MaxJobs = 10000
	}
	if cfg.MaxActive <= 0 {
		cfg.MaxActive = 16
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	cfg.SigningKey = append(ed25519.PrivateKey(nil), cfg.SigningKey...)
	persistent, err := journal.Open(context.Background(), cfg.Database, journal.Broker)
	if err != nil {
		return nil, err
	}
	if _, err = persistent.Conn.ExecContext(context.Background(), "CREATE TABLE IF NOT EXISTS jobs (id TEXT PRIMARY KEY, scope TEXT NOT NULL, principal TEXT NOT NULL, request_key TEXT NOT NULL, body BLOB NOT NULL, UNIQUE(scope,principal,request_key))"); err != nil {
		return nil, errors.Join(err, persistent.Close())
	}
	images := make(map[string]bool, len(cfg.Images))
	for _, value := range cfg.Images {
		images[value] = true
	}
	return &Broker{db: persistent.DB, conn: persistent.Conn, cfg: cfg, images: images}, nil
}

// Close releases the journal. It does not terminate persisted workloads.
func (b *Broker) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return errors.Join(b.conn.Close(), b.db.Close())
}

// PublicKey returns a detached receipt verification key for authenticated hosts.
func (b *Broker) PublicKey() ed25519.PublicKey {
	return append(ed25519.PublicKey(nil), b.cfg.SigningKey[ed25519.SeedSize:]...)
}

// Capabilities returns the runtime's deployment attestation to an authorized caller.
func (b *Broker) Capabilities(ctx context.Context) (sandbox.Capabilities, error) {
	if _, err := caller(ctx); err != nil {
		return sandbox.Capabilities{}, err
	}
	return b.cfg.Runtime.Capabilities(ctx)
}

func (b *Broker) authorize(ctx context.Context, j sandbox.Job) error {
	who, err := caller(ctx)
	if err != nil {
		return err
	}
	if who.Scope.Canonical() != j.Scope.Canonical() || who.Principal != j.Principal {
		return ErrDenied
	}
	detached, err := sandbox.Seal(j)
	if err != nil {
		return err
	}
	if operationErr := b.cfg.Authorize(ctx, who, detached); operationErr != nil {
		return operationErr
	}
	return ctx.Err()
}

// Submit persists a signed receipt before the first runtime call. Retry the same
// sealed job after an ambiguous error; the runtime must preserve its ID history.
func (b *Broker) Submit(ctx context.Context, j sandbox.Job) (sandbox.Receipt, error) {
	if err := j.Validate(); err != nil {
		return sandbox.Receipt{}, err
	}
	frozen, err := sandbox.Seal(j)
	if err != nil {
		return sandbox.Receipt{}, err
	}
	j = frozen
	if operationErr := b.authorize(ctx, j); operationErr != nil {
		return sandbox.Receipt{}, operationErr
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	prior, err := b.byKey(ctx, j)
	if err == nil {
		if prior.Status.Receipt.Job.Digest != j.Digest {
			return sandbox.Receipt{}, ErrConflict
		}
		if prior.Status.State == sandbox.StateAccepted && !prior.Cancelling {
			if operationErr := b.start(ctx, &prior); operationErr != nil {
				return sandbox.Receipt{}, operationErr
			}
		}
		return prior.Status.Receipt, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return sandbox.Receipt{}, err
	}
	var total, active int
	if operationErr := b.conn.QueryRowContext(ctx, "SELECT count(*),coalesce(sum(CASE WHEN json_extract(body,'$.status.state') IN ('accepted','running') THEN 1 ELSE 0 END),0) FROM jobs").Scan(&total, &active); operationErr != nil {
		return sandbox.Receipt{}, operationErr
	}
	if total >= b.cfg.MaxJobs || active >= b.cfg.MaxActive {
		return sandbox.Receipt{}, fmt.Errorf("cortex: sandbox admission capacity reached")
	}
	if !b.images[j.Image] {
		return sandbox.Receipt{}, ErrDenied
	}
	caps, err := b.cfg.Runtime.Capabilities(ctx)
	if err != nil {
		return sandbox.Receipt{}, err
	}
	if operationErr := caps.Supports(j); operationErr != nil {
		return sandbox.Receipt{}, operationErr
	}
	if operationErr := b.authorize(ctx, j); operationErr != nil {
		return sandbox.Receipt{}, operationErr
	}
	acceptedAt := b.cfg.Now().UTC()
	if acceptedAt.IsZero() {
		return sandbox.Receipt{}, fmt.Errorf("cortex: missing sandbox acceptance time")
	}
	receipt := sandbox.Receipt{SchemaVersion: 1, Job: j, Capabilities: caps, Broker: b.cfg.Name, AcceptedAt: acceptedAt}
	data, err := json.Marshal(receipt)
	if err != nil {
		return sandbox.Receipt{}, err
	}
	receipt.Signature = ed25519.Sign(b.cfg.SigningKey, data)
	rec := record{Status: sandbox.JobStatus{Receipt: receipt, State: sandbox.StateAccepted, Revision: 1, UpdatedAt: receipt.AcceptedAt}}
	body, err := json.Marshal(rec)
	if err != nil {
		return sandbox.Receipt{}, err
	}
	if _, err = b.conn.ExecContext(ctx, "INSERT INTO jobs(id,scope,principal,request_key,body) VALUES(?,?,?,?,?)", j.ID.String(), j.Scope.Canonical(), j.Principal, j.Key, body); err != nil {
		var exists int
		if lookupErr := b.conn.QueryRowContext(ctx, "SELECT 1 FROM jobs WHERE id=?", j.ID.String()).Scan(&exists); lookupErr == nil {
			return sandbox.Receipt{}, ErrConflict
		}
		return sandbox.Receipt{}, err
	}
	if operationErr := b.start(ctx, &rec); operationErr != nil {
		return sandbox.Receipt{}, operationErr
	}
	return receipt, nil
}

func (b *Broker) start(ctx context.Context, rec *record) error {
	j := rec.Status.Receipt.Job
	if err := bindingCheck(rec, b.PublicKey()); err != nil {
		return err
	}
	if err := b.authorize(ctx, j); err != nil {
		return err
	}
	caps, err := b.cfg.Runtime.Capabilities(ctx)
	if err != nil {
		return err
	}
	if operationErr := caps.Supports(j); operationErr != nil {
		return operationErr
	}
	accepted := rec.Status.Receipt.Capabilities
	if caps.RuntimeDigest != accepted.RuntimeDigest || caps.Profile != accepted.Profile || caps.Qualification != accepted.Qualification {
		return ErrConflict
	}
	if operationErr := b.authorize(ctx, j); operationErr != nil {
		return operationErr
	}
	detachedJob, err := sandbox.Seal(j)
	if err != nil {
		return err
	}
	now := b.cfg.Now()
	if now.IsZero() {
		return fmt.Errorf("cortex: missing sandbox dispatch time")
	}
	if !now.Before(rec.Status.Receipt.AcceptedAt.Add(j.Limits.WallTime)) {
		rec.Cancelling = true
		if operationErr := b.save(ctx, rec); operationErr != nil {
			return operationErr
		}
		return errors.Join(context.DeadlineExceeded, b.refresh(ctx, rec))
	}
	detached := rec.Status.Receipt
	detached.Job = detachedJob
	detached.Signature = append([]byte(nil), detached.Signature...)
	if operationErr := b.cfg.Runtime.Start(ctx, detached); operationErr != nil {
		return fmt.Errorf("cortex: sandbox start uncertain; inspect same job: %w", operationErr)
	}
	rec.Status.State = sandbox.StateRunning
	return b.save(ctx, rec)
}

// Inspect reconciles the durable record with the isolated runtime.
func (b *Broker) Inspect(ctx context.Context, key id.ID) (sandbox.JobStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rec, err := b.load(ctx, key)
	if err != nil {
		return sandbox.JobStatus{}, err
	}
	if operationErr := b.refresh(ctx, &rec); operationErr != nil {
		return sandbox.JobStatus{}, operationErr
	}
	return rec.Status, nil
}

// Cancel records cancellation intent before contacting the runtime. A restart or
// cancelled request cannot make that job eligible to start again.
func (b *Broker) Cancel(ctx context.Context, key id.ID) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	rec, err := b.load(ctx, key)
	if err != nil {
		return err
	}
	if rec.Result != nil {
		return nil
	}
	rec.Cancelling = true
	if operationErr := b.save(ctx, &rec); operationErr != nil {
		return operationErr
	}
	return b.refresh(ctx, &rec)
}

// Collect returns only a persisted terminal result and rechecks current grants.
func (b *Broker) Collect(ctx context.Context, key id.ID) (sandbox.Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rec, err := b.load(ctx, key)
	if err != nil {
		return sandbox.Result{}, err
	}
	if operationErr := b.refresh(ctx, &rec); operationErr != nil {
		return sandbox.Result{}, operationErr
	}
	if rec.Result == nil {
		return sandbox.Result{}, ErrPending
	}
	return *rec.Result, nil
}

func (b *Broker) refresh(ctx context.Context, rec *record) error {
	if rec.Result != nil {
		return nil
	}
	if rec.Cancelling {
		if err := b.stop(ctx, rec); err != nil {
			return err
		}
	}
	result, err := b.observe(ctx, rec)
	if result == nil && err == nil {
		return nil
	}
	if err != nil {
		return err
	}
	if operationErr := ValidateRuntimeResult(*result, rec.Status.Receipt.Job); operationErr != nil {
		return operationErr
	}
	if operationErr := b.authorize(ctx, rec.Status.Receipt.Job); operationErr != nil {
		return operationErr
	}
	rec.Status.State = result.State
	if result.State != sandbox.StateRunning {
		if operationErr := b.signResult(result); operationErr != nil {
			return operationErr
		}
		rec.Result = result
	}
	return b.save(ctx, rec)
}

// ValidateRuntimeResult checks bounded runtime output against a sealed job.
// It establishes neither signature authenticity nor task correctness.
func ValidateRuntimeResult(result sandbox.Result, j sandbox.Job) error {
	if err := j.Validate(); err != nil {
		return err
	}
	if result.JobDigest != j.Digest || len(result.Reason) > 4096 || int64(len(result.Stdout))+int64(len(result.Stderr)) > j.Limits.OutputBytes || len(result.Artifacts) > 256 {
		return fmt.Errorf("cortex: invalid sandbox runtime result")
	}
	if result.State == sandbox.StateExited && (result.ExitCode < 0 || result.ExitCode > 255) {
		return fmt.Errorf("cortex: invalid sandbox exit status")
	}
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > 2<<20 {
		return fmt.Errorf("cortex: sandbox result exceeds encoding bound")
	}
	switch result.State {
	case sandbox.StateRunning, sandbox.StateExited, sandbox.StateCancelled, sandbox.StateFailed:
	default:
		return fmt.Errorf("cortex: invalid sandbox result state")
	}
	for _, ref := range result.Artifacts {
		if err := ref.Validate(); err != nil {
			return err
		}
		if ref.Scope.Canonical() != j.Scope.Canonical() {
			return ErrDenied
		}
	}
	return nil
}

func (b *Broker) load(ctx context.Context, key id.ID) (record, error) {
	who, err := caller(ctx)
	if err != nil {
		return record{}, err
	}
	if key.IsNil() || key.Prefix() != sandbox.PrefixJob {
		return record{}, ErrNotFound
	}
	return b.read(ctx, "SELECT body FROM jobs WHERE id=? AND scope=? AND principal=?", key.String(), who.Scope.Canonical(), who.Principal)
}
func (b *Broker) byKey(ctx context.Context, j sandbox.Job) (record, error) {
	return b.read(ctx, "SELECT body FROM jobs WHERE scope=? AND principal=? AND request_key=?", j.Scope.Canonical(), j.Principal, j.Key)
}
func (b *Broker) read(ctx context.Context, query string, args ...any) (record, error) {
	var body []byte
	if err := b.conn.QueryRowContext(ctx, query, args...).Scan(&body); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return record{}, ErrNotFound
		}
		return record{}, err
	}
	var rec record
	if err := json.Unmarshal(body, &rec); err != nil {
		return record{}, err
	}
	if err := bindingCheck(&rec, b.PublicKey()); err != nil {
		return record{}, err
	}
	if err := b.authorize(ctx, rec.Status.Receipt.Job); err != nil {
		return record{}, err
	}
	return rec, nil
}
func bindingCheck(rec *record, key ed25519.PublicKey) error {
	if err := VerifyReceipt(rec.Status.Receipt, key); err != nil {
		return err
	}
	if rec.Result != nil {
		if rec.Status.State != rec.Result.State {
			return ErrConflict
		}
		return VerifyResult(*rec.Result, rec.Status.Receipt.Job, key)
	}
	return nil
}
func (b *Broker) save(ctx context.Context, rec *record) error {
	rec.Status.Revision++
	rec.Status.UpdatedAt = b.cfg.Now().UTC()
	if rec.Status.UpdatedAt.IsZero() {
		return fmt.Errorf("cortex: missing sandbox observation time")
	}
	body, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	j := rec.Status.Receipt.Job
	_, err = b.conn.ExecContext(ctx, "UPDATE jobs SET body=? WHERE id=? AND scope=? AND principal=?", body, j.ID.String(), j.Scope.Canonical(), j.Principal)
	return err
}

// VerifyReceipt checks signed provenance and the complete job binding. Pin the
// public key through host configuration, never through the response itself.
func VerifyReceipt(receipt sandbox.Receipt, key ed25519.PublicKey) error {
	if len(key) != ed25519.PublicKeySize || receipt.SchemaVersion != 1 || receipt.Broker == "" || receipt.AcceptedAt.IsZero() {
		return ErrDenied
	}
	if err := receipt.Job.Validate(); err != nil {
		return err
	}
	signature := receipt.Signature
	receipt.Signature = nil
	data, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	if !ed25519.Verify(key, data, signature) {
		return ErrDenied
	}
	return nil
}

func (b *Broker) signResult(result *sandbox.Result) error {
	result.Signature = nil
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	result.Signature = ed25519.Sign(b.cfg.SigningKey, data)
	return nil
}

// VerifyResult authenticates a terminal result against its sealed job.
func VerifyResult(result sandbox.Result, j sandbox.Job, key ed25519.PublicKey) error {
	if len(key) != ed25519.PublicKeySize {
		return ErrDenied
	}
	if err := j.Validate(); err != nil {
		return err
	}
	if err := ValidateRuntimeResult(result, j); err != nil {
		return err
	}
	if result.State == sandbox.StateRunning {
		return ErrPending
	}
	signature := result.Signature
	result.Signature = nil
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if !ed25519.Verify(key, data, signature) {
		return ErrDenied
	}
	return nil
}

func (b *Broker) stop(ctx context.Context, rec *record) error {
	err := b.cfg.Runtime.Cancel(ctx, rec.Status.Receipt.Job.ID)
	if errors.Is(err, ErrNotFound) && rec.Status.State == sandbox.StateAccepted {
		return ctx.Err()
	}
	return err
}
func (b *Broker) observe(ctx context.Context, rec *record) (*sandbox.Result, error) {
	result, err := b.cfg.Runtime.Inspect(ctx, rec.Status.Receipt.Job.ID)
	if errors.Is(err, ErrNotFound) && rec.Status.State == sandbox.StateAccepted {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !rec.Cancelling {
			return nil, nil
		}
		return &sandbox.Result{JobDigest: rec.Status.Receipt.Job.Digest, State: sandbox.StateCancelled, Reason: "cancelled_before_start"}, nil
	}
	return &result, err
}
