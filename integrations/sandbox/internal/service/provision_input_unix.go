//go:build linux || darwin

package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
	"github.com/xraph/cortex/sandbox"
)

// InputProvisionRequest selects an exact declared guest input from a protected
// canonical job file. Config and Job are mode-0600 files; File is the mode-0600
// source whose bytes must match that declaration. No path is fetched remotely.
type InputProvisionRequest struct {
	Config string
	Job    string
	Input  string
	File   string
}

// InputInstallation identifies a verified installed input without disclosing
// its content, principal or source URI. It grants no execution or qualification.
type InputInstallation struct {
	SchemaVersion int    `json:"schema_version"`
	JobID         id.ID  `json:"job_id"`
	JobDigest     string `json:"job_digest"`
	BindingDigest string `json:"binding_digest"`
	SHA256        string `json:"sha256"`
	Bytes         int64  `json:"bytes"`
}

// ProvisionInput installs one authorized input on a Linux host as root. It
// opens no database, listener or worker service. Identical retries verify the
// existing file; conflicts leave it untouched. An error after publication can
// leave complete bytes installed, so retry the same request to resolve ambiguity.
func ProvisionInput(ctx context.Context, request InputProvisionRequest) (InputInstallation, error) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return InputInstallation{}, errUnsupported
	}
	return provisionInput(ctx, request, 0)
}

func provisionInput(ctx context.Context, request InputProvisionRequest, owner uint32) (InputInstallation, error) {
	operation, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	publication, err := prepareInput(operation, request, owner)
	if err != nil {
		return InputInstallation{}, err
	}
	return publication.publish(operation)
}

type inputPublication struct {
	request           InputProvisionRequest
	owner             uint32
	configSHA, jobSHA string
	source            *LocalInputs
	job               sandbox.Job
	ref               evidence.Ref
	data              []byte
	receipt           InputInstallation
}

func prepareInput(ctx context.Context, request InputProvisionRequest, owner uint32) (*inputPublication, error) {
	if !profilePath(request.Config) || !profilePath(request.Job) || !profilePath(request.File) || request.Input == "" {
		return nil, fmt.Errorf("cortex: invalid input provisioning request")
	}
	config, err := readBrokerConfig(ctx, request.Config, owner)
	if err != nil {
		return nil, err
	}
	body, err := hostfs.ReadOwned(ctx, request.Job, owner, 0o600, 1<<20)
	if err != nil {
		return nil, err
	}
	var job sandbox.Job
	if decodeCanonical(body, &job) != nil || job.Validate() != nil {
		return nil, fmt.Errorf("cortex: invalid provisioning job")
	}
	var ref evidence.Ref
	for _, input := range job.Inputs {
		if input.Path == request.Input {
			ref = detachedCaptureRef(input.Ref)
			break
		}
	}
	if ref.ID.IsNil() {
		return nil, evidence.ErrDenied
	}
	policy, err := newEvidencePolicy(ctx, config.Settings.EvidencePolicy, owner, time.Now)
	if err != nil {
		return nil, err
	}
	source, err := newLocalInputs(config.Settings.InputRoot, config.Settings.EvidenceBytes, config.Settings.InputReaders, policy.AuthorizeInput, owner)
	if err != nil {
		return nil, err
	}
	if policyErr := source.authorize(evidenceContext(ctx, job), ref); policyErr != nil {
		return nil, policyErr
	}
	data, err := hostfs.ReadPin(ctx, hostfs.Pin{Path: request.File, SHA256: ref.SHA256, Bytes: ref.Bytes, Mode: 0o600}, owner)
	if err != nil {
		return nil, err
	}
	binding, err := ref.BindingDigest()
	if err != nil {
		return nil, err
	}
	return &inputPublication{request: request, owner: owner, configSHA: config.SHA256, jobSHA: evidence.Digest(body), source: source, job: job, ref: ref, data: data, receipt: InputInstallation{SchemaVersion: 1, JobID: job.ID, JobDigest: job.Digest, BindingDigest: binding, SHA256: ref.SHA256, Bytes: ref.Bytes}}, nil
}

func (p *inputPublication) current(ctx context.Context) error {
	for _, file := range []struct{ name, digest string }{{p.request.Config, p.configSHA}, {p.request.Job, p.jobSHA}} {
		body, err := hostfs.ReadOwned(ctx, file.name, p.owner, 0o600, 1<<20)
		if err != nil {
			return err
		}
		if evidence.Digest(body) != file.digest {
			return broker.ErrConflict
		}
	}
	if _, err := p.source.checkDirectory(); err != nil {
		return err
	}
	return p.source.authorize(evidenceContext(ctx, p.job), p.ref)
}

func (p *inputPublication) publish(ctx context.Context) (InputInstallation, error) {
	if err := p.current(ctx); err != nil {
		return InputInstallation{}, err
	}
	pin := hostfs.Pin{Path: filepath.Join(p.source.root, p.receipt.BindingDigest), SHA256: p.ref.SHA256, Bytes: p.ref.Bytes, Mode: 0o400}
	if err := hostfs.PublishPin(ctx, pin, p.owner, p.data, func() error { return p.current(ctx) }); err != nil {
		return InputInstallation{}, err
	}
	reader, err := p.source.Open(evidenceContext(ctx, p.job), p.ref)
	if err != nil {
		return InputInstallation{}, err
	}
	if closeErr := errors.Join(reader.Close(), p.current(ctx)); closeErr != nil {
		return InputInstallation{}, closeErr
	}
	return p.receipt, nil
}
