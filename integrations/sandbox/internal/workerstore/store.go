// Package workerstore persists worker ownership without granting execution.
package workerstore

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/acceptance"
	"github.com/xraph/cortex/integrations/sandbox/internal/journal"
	"github.com/xraph/cortex/sandbox"
)

// ErrConflict prevents identity reuse and stale worker or reaper writes.
var ErrConflict = broker.ErrConflict

// ErrCorrupt means persisted state cannot establish a safe ownership decision.
var ErrCorrupt = errors.New("cortex: corrupt sandbox worker journal")

// RecordVersion is the current worker ownership record format.
const RecordVersion = 2

// JournalVersion includes the separately revisioned artifact capture table.
// Older daemons refuse this version instead of ignoring pending captures.
const JournalVersion = 3

// Phase is the durable ownership state, not a correctness verdict.
type Phase string

const (
	// Accepted is an enrolled job that has never granted a worker claim.
	Accepted Phase = "accepted"
	// Claimed means execution may have started, even after a lost response.
	Claimed Phase = "claimed"
	// Reaping fences worker completion until external containment is stopped.
	Reaping Phase = "reaping"
	// Terminal permanently prevents another claim for this identity.
	Terminal Phase = "terminal"
)

// Config binds a journal to one trusted broker identity. Keep the database and
// its private directory for the lifetime of accepted job identities. MaxJobs
// includes terminal tombstones; there is no automatic deletion or rollover.
type Config struct {
	Database, Broker string
	PublicKey        ed25519.PublicKey
	MaxJobs          int
	Now              func() time.Time
}

// Record is an immutable acceptance plus monotonic worker state. TokenHash never
// reveals the worker capability; losing the Claim response requires reaping.
type Record struct {
	SchemaVersion   int             `json:"schema_version"`
	Receipt         sandbox.Receipt `json:"receipt"`
	Phase           Phase           `json:"phase"`
	Revision        uint64          `json:"revision"`
	TokenHash       string          `json:"token_hash,omitempty"`
	CancelRequested bool            `json:"cancel_requested"`
	Result          *sandbox.Result `json:"result,omitempty"`
	Proposal        *Proposal       `json:"proposal,omitempty"`
}

// Claim carries the single-use execution ownership grant. The caller must
// authenticate and supervise the worker; this capability does not launch it.
type Claim struct {
	Record Record
	Token  string
}

// PendingJob is a bounded recovery index. Load verifies its full receipt before
// a reaper acts; external containment discovery must also handle journal faults.
type PendingJob struct {
	ID       id.ID
	Phase    Phase
	Revision uint64
}

// Store serializes one daemon's transitions under an exclusive process lock.
// Other processes cannot open the journal concurrently. Workers communicate
// through the owning daemon, not by opening this database themselves.
type Store struct {
	mu      sync.Mutex
	journal *journal.Journal
	cfg     Config
	closed  bool
}

// Open acquires durable ownership and verifies the schema and broker key pin.
func Open(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.Broker == "" || len(cfg.Broker) > 512 || len(cfg.PublicKey) != ed25519.PublicKeySize || cfg.MaxJobs <= 0 || cfg.MaxJobs > 100_000 {
		return nil, fmt.Errorf("cortex: invalid worker journal configuration")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	cfg.PublicKey = append(ed25519.PublicKey(nil), cfg.PublicKey...)
	persistent, err := journal.Open(ctx, cfg.Database, journal.Worker)
	if err != nil {
		return nil, err
	}
	s := &Store{journal: persistent, cfg: cfg}
	if err = s.migrate(ctx); err != nil {
		return nil, errors.Join(err, persistent.Close())
	}
	return s, nil
}

func (s *Store) migrate(ctx context.Context) (err error) {
	var version int
	if queryErr := s.journal.Conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); queryErr != nil {
		return queryErr
	}
	if version < 0 || version > JournalVersion {
		return ErrCorrupt
	}
	if version == 0 {
		var tables int
		if queryErr := s.journal.Conn.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'").Scan(&tables); queryErr != nil {
			return queryErr
		}
		if tables != 0 {
			return ErrCorrupt
		}
	}
	tx, err := s.journal.Conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, rollbackErr)
		}
	}()
	if version == 0 {
		for _, statement := range []string{
			"CREATE TABLE worker_identity (id INTEGER PRIMARY KEY CHECK(id=1), broker TEXT NOT NULL, key_hash TEXT NOT NULL)",
			"CREATE TABLE worker_jobs (id TEXT PRIMARY KEY, phase TEXT NOT NULL, revision INTEGER NOT NULL, body BLOB NOT NULL)",
			"PRAGMA user_version=1",
		} {
			if _, err = tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO worker_identity(id,broker,key_hash) VALUES(1,?,?)", s.cfg.Broker, evidence.Digest(s.cfg.PublicKey)); err != nil {
			return err
		}
	}
	var brokerName, keyHash string
	if operationErr := tx.QueryRowContext(ctx, "SELECT broker,key_hash FROM worker_identity WHERE id=1").Scan(&brokerName, &keyHash); operationErr != nil {
		return operationErr
	}
	if brokerName != s.cfg.Broker || keyHash != evidence.Digest(s.cfg.PublicKey) {
		return ErrConflict
	}
	if version < JournalVersion {
		var tables int
		if queryErr := tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='worker_captures'").Scan(&tables); queryErr != nil {
			return queryErr
		}
		if tables == 0 {
			if _, err = tx.ExecContext(ctx, captureTableSQL); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, "PRAGMA user_version=3"); err != nil {
			return err
		}
	}
	// Pin the actual constraints as well as the version marker. A table without
	// the compound primary key could admit multiple bindings for one output.
	var captureSchema string
	if queryErr := tx.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type='table' AND name='worker_captures'").Scan(&captureSchema); queryErr != nil {
		return errors.Join(ErrCorrupt, queryErr)
	}
	if captureSchema != captureTableSQL {
		return ErrCorrupt
	}
	return tx.Commit()
}

// Close releases ownership without cancelling or forgetting accepted work.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.journal.Close()
}

// CheckBroker binds daemon assembly to the journal's pinned broker identity.
// It reads immutable configuration and does not grant execution or user access.
func (s *Store) CheckBroker(name string, key ed25519.PublicKey) error {
	if s == nil || name != s.cfg.Broker || len(key) != ed25519.PublicKeySize || !bytes.Equal(key, s.cfg.PublicKey) {
		return ErrConflict
	}
	return nil
}

// Enroll persists the exact signed acceptance before a worker can claim it.
// Identical retries return the original record, including terminal tombstones.
func (s *Store) Enroll(ctx context.Context, receipt sandbox.Receipt) (Record, error) {
	if err := s.receipt(receipt); err != nil {
		return Record{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prior, err := s.load(ctx, receipt.Job.ID)
	if err == nil {
		left, marshalErr := json.Marshal(prior.Receipt)
		if marshalErr != nil {
			return Record{}, marshalErr
		}
		right, marshalErr := json.Marshal(receipt)
		if marshalErr != nil {
			return Record{}, marshalErr
		}
		if !bytes.Equal(left, right) {
			return Record{}, ErrConflict
		}
		return prior, nil
	}
	if !errors.Is(err, broker.ErrNotFound) {
		return Record{}, err
	}
	if operationErr := s.admit(receipt); operationErr != nil {
		return Record{}, operationErr
	}
	var total int
	if operationErr := s.journal.Conn.QueryRowContext(ctx, "SELECT count(*) FROM worker_jobs").Scan(&total); operationErr != nil {
		return Record{}, operationErr
	}
	if total >= s.cfg.MaxJobs {
		return Record{}, fmt.Errorf("cortex: worker identity capacity reached")
	}
	record := Record{SchemaVersion: RecordVersion, Receipt: receipt, Phase: Accepted, Revision: 1}
	body, err := encode(record)
	if err != nil {
		return Record{}, err
	}
	if _, err = s.journal.Conn.ExecContext(ctx, "INSERT INTO worker_jobs(id,phase,revision,body) VALUES(?,?,?,?)", receipt.Job.ID.String(), record.Phase, record.Revision, body); err != nil {
		return Record{}, err
	}
	return s.load(ctx, receipt.Job.ID)
}

// Claim grants ownership once. Neither expiration, process exit nor a lost
// acknowledgement makes a claimed identity runnable again.
func (s *Store) Claim(ctx context.Context, key id.ID) (Claim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.load(ctx, key)
	if err != nil {
		return Claim{}, err
	}
	if record.Phase != Accepted {
		return Claim{}, ErrConflict
	}
	if err = s.admit(record.Receipt); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			record.CancelRequested = true
			record.Phase = Terminal
			record.Result = &sandbox.Result{JobDigest: record.Receipt.Job.Digest, State: sandbox.StateCancelled, ExitCode: -1, Reason: "acceptance deadline expired before claim"}
			if saveErr := s.save(ctx, &record); saveErr != nil {
				return Claim{}, errors.Join(err, saveErr)
			}
		}
		return Claim{}, err
	}
	var random [32]byte
	if _, err = rand.Read(random[:]); err != nil {
		return Claim{}, err
	}
	token := hex.EncodeToString(random[:])
	record.TokenHash = evidence.Digest([]byte(token))
	record.Phase = Claimed
	if operationErr := s.save(ctx, &record); operationErr != nil {
		return Claim{}, operationErr
	}
	return Claim{Record: record, Token: token}, nil
}

// VerifyClaim checks current ownership without issuing another execution grant.
// The daemon must authenticate the worker's service identity and recheck user
// grants separately. Cancellation, reaping, completion and expiration invalidate
// an old credential even when its signed receipt remains cryptographically valid.
func (s *Store) VerifyClaim(ctx context.Context, key id.ID, token string, revision uint64) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.load(ctx, key)
	if err != nil {
		return Record{}, err
	}
	if record.Phase != Claimed || record.Revision != revision || !matchesToken(record, token) {
		return Record{}, ErrConflict
	}
	if operationErr := s.admit(record.Receipt); operationErr != nil {
		return Record{}, operationErr
	}
	if operationErr := ctx.Err(); operationErr != nil {
		return Record{}, operationErr
	}
	return record, nil
}

func matchesToken(record Record, token string) bool {
	if len(token) != 64 {
		return false
	}
	hash := evidence.Digest([]byte(token))
	return subtle.ConstantTimeCompare([]byte(hash), []byte(record.TokenHash)) == 1
}

// Cancel durably prevents a future claim. Claimed work remains nonterminal
// until the external reaper proves that every descendant has stopped.
func (s *Store) Cancel(ctx context.Context, key id.ID) (Record, error) {
	return s.requireReap(ctx, key, true)
}

// RequireReap fences a lost worker without authorizing another execution.
func (s *Store) RequireReap(ctx context.Context, key id.ID) (Record, error) {
	return s.requireReap(ctx, key, false)
}

func (s *Store) requireReap(ctx context.Context, key id.ID, cancel bool) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.load(ctx, key)
	if err != nil {
		return Record{}, err
	}
	if record.Phase == Terminal || (record.Phase == Reaping && (!cancel || record.CancelRequested)) {
		return record, nil
	}
	if record.Phase == Accepted && !cancel {
		return Record{}, ErrConflict
	}
	record.CancelRequested = record.CancelRequested || cancel
	if record.Phase == Accepted {
		record.Phase = Terminal
		record.Result = &sandbox.Result{JobDigest: record.Receipt.Job.Digest, State: sandbox.StateCancelled, ExitCode: -1, Reason: "cancelled before claim"}
	} else {
		record.Phase = Reaping
		if record.Proposal != nil {
			record.Proposal.Discarded = true
		}
	}
	if operationErr := s.save(ctx, &record); operationErr != nil {
		return Record{}, operationErr
	}
	return record, nil
}

// Reaped records a trusted external containment proof at the expected revision.
// The caller must independently stop the worker and sandbox cgroups, including
// ambiguous launches. An absent runtime state file alone is not proof.
func (s *Store) Reaped(ctx context.Context, key id.ID, expected uint64) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.load(ctx, key)
	if err != nil {
		return Record{}, err
	}
	if record.Phase != Reaping || record.Revision != expected {
		return Record{}, ErrConflict
	}
	state := sandbox.StateFailed
	if record.CancelRequested {
		state = sandbox.StateCancelled
	}
	record.Phase = Terminal
	record.Result = &sandbox.Result{JobDigest: record.Receipt.Job.Digest, State: state, ExitCode: -1, Reason: "external reaper confirmed containment stopped"}
	if record.Proposal != nil {
		record.Proposal.Result = nil
	}
	if operationErr := s.save(ctx, &record); operationErr != nil {
		return Record{}, operationErr
	}
	return record, nil
}

// Load returns detached state after checking the receipt and record bindings.
// This internal operation is not an authorized user-facing read endpoint.
func (s *Store) Load(ctx context.Context, key id.ID) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load(ctx, key)
}

func (s *Store) load(ctx context.Context, key id.ID) (Record, error) {
	if key.IsNil() || key.Prefix() != sandbox.PrefixJob {
		return Record{}, broker.ErrNotFound
	}
	var body []byte
	var phase Phase
	var revision uint64
	err := s.journal.Conn.QueryRowContext(ctx, "SELECT phase,revision,CASE WHEN length(body)<=4194304 THEN body ELSE NULL END FROM worker_jobs WHERE id=?", key.String()).Scan(&phase, &revision, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, broker.ErrNotFound
	}
	if err != nil {
		return Record{}, err
	}
	var record Record
	if err = json.Unmarshal(body, &record); err != nil {
		return Record{}, errors.Join(ErrCorrupt, err)
	}
	if record.Receipt.Job.ID != key || record.Phase != phase || record.Revision != revision {
		return Record{}, ErrCorrupt
	}
	if err = s.validate(record); err != nil {
		return Record{}, errors.Join(ErrCorrupt, err)
	}
	return record, nil
}

func (s *Store) save(ctx context.Context, record *Record) error {
	previous := record.Revision
	if previous >= math.MaxInt64 {
		return ErrConflict
	}
	record.Revision++
	record.SchemaVersion = RecordVersion
	if err := s.validate(*record); err != nil {
		return err
	}
	body, err := encode(*record)
	if err != nil {
		return err
	}
	result, err := s.journal.Conn.ExecContext(ctx, "UPDATE worker_jobs SET phase=?,revision=?,body=? WHERE id=? AND revision=?", record.Phase, record.Revision, body, record.Receipt.Job.ID.String(), previous)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrConflict
	}
	return nil
}

func (s *Store) receipt(receipt sandbox.Receipt) error {
	if receipt.Broker != s.cfg.Broker {
		return broker.ErrDenied
	}
	return broker.VerifyReceipt(receipt, s.cfg.PublicKey)
}

func (s *Store) admit(receipt sandbox.Receipt) error {
	now := s.cfg.Now()
	if now.IsZero() || now.Before(receipt.AcceptedAt) {
		return fmt.Errorf("cortex: invalid worker acceptance time")
	}
	if !now.Before(acceptance.Deadline(receipt)) {
		return context.DeadlineExceeded
	}
	return receipt.Capabilities.Supports(receipt.Job)
}

func (s *Store) validate(record Record) error {
	if (record.SchemaVersion != 1 && record.SchemaVersion != RecordVersion) || record.Revision == 0 || record.Revision > math.MaxInt64 {
		return ErrCorrupt
	}
	if err := s.receipt(record.Receipt); err != nil {
		return err
	}
	if record.SchemaVersion == 1 && (record.Proposal != nil || record.Phase == Finalizing) {
		return ErrCorrupt
	}
	switch record.Phase {
	case Accepted:
		if record.TokenHash != "" || record.CancelRequested || record.Result != nil || record.Proposal != nil {
			return ErrCorrupt
		}
	case Claimed, Reaping:
		if !evidence.ValidDigest(record.TokenHash) || record.Result != nil || (record.Phase == Claimed && (record.CancelRequested || record.Proposal != nil)) {
			return ErrCorrupt
		}
	case Finalizing:
		if record.SchemaVersion != RecordVersion || !evidence.ValidDigest(record.TokenHash) || record.CancelRequested || record.Result != nil || record.Proposal == nil || record.Proposal.ClaimRevision >= record.Revision || record.Revision-record.Proposal.ClaimRevision != 1 {
			return ErrCorrupt
		}
	case Terminal:
		if record.Result == nil || (record.TokenHash != "" && !evidence.ValidDigest(record.TokenHash)) || (record.CancelRequested && record.Result.State != sandbox.StateCancelled) || (record.TokenHash == "" && !record.CancelRequested) {
			return ErrCorrupt
		}
		if record.SchemaVersion == RecordVersion && record.Result.State == sandbox.StateExited && record.Proposal == nil {
			return ErrCorrupt
		}
		if resultErr := terminalResult(*record.Result, record.Receipt.Job); resultErr != nil {
			return resultErr
		}
	default:
		return ErrCorrupt
	}
	return validateProposal(record)
}

func terminalResult(result sandbox.Result, job sandbox.Job) error {
	if result.State == sandbox.StateRunning || len(result.Signature) != 0 {
		return ErrConflict
	}
	return broker.ValidateRuntimeResult(result, job)
}

func encode(record Record) ([]byte, error) {
	body, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	if len(body) > 4<<20 {
		return nil, fmt.Errorf("cortex: worker record exceeds storage bound")
	}
	return body, nil
}

// Pending returns at most 256 lightweight recovery entries after an ID cursor.
func (s *Store) Pending(ctx context.Context, after string, limit int) ([]PendingJob, error) {
	if limit < 1 || limit > 256 {
		return nil, fmt.Errorf("cortex: invalid worker recovery page size")
	}
	if after != "" {
		parsed, err := id.Parse(after)
		if err != nil || parsed.Prefix() != sandbox.PrefixJob {
			return nil, fmt.Errorf("cortex: invalid worker recovery cursor")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.journal.Conn.QueryContext(ctx, "SELECT id,phase,revision FROM worker_jobs WHERE id>? AND phase<>? ORDER BY id LIMIT ?", after, Terminal, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []PendingJob
	for rows.Next() {
		var name string
		var entry PendingJob
		if operationErr := rows.Scan(&name, &entry.Phase, &entry.Revision); operationErr != nil {
			return nil, operationErr
		}
		entry.ID, err = id.Parse(name)
		if err != nil || entry.ID.Prefix() != sandbox.PrefixJob || entry.Revision == 0 || entry.Revision > math.MaxInt64 || (entry.Phase != Accepted && entry.Phase != Claimed && entry.Phase != Reaping && entry.Phase != Finalizing) {
			return nil, ErrCorrupt
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}
