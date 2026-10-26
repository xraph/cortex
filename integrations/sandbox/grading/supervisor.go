package grading

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"time"
	"unicode/utf8"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/sandbox"
	"github.com/xraph/cortex/verification"
)

// EqualityProtocol selects bounded named JSON equality checks. It does not run
// arbitrary tests or interpret candidate text as supervisor instructions.
const EqualityProtocol = "cortex.json-equality.v1"

// EqualitySupervisorRevision pins the implementation of the equality protocol.
const EqualitySupervisorRevision = "865713cfa2ef0a3ec50726f34a7d748bca37e9b44d5b9bd905478e5a58fd1474"

// SupervisorRegistration reserves the immutable assessment identity for a manifest.
// The host must retain this registration across process restarts.
type SupervisorRegistration struct {
	Manifest     verification.EvaluationManifest
	AssessmentID id.ID
}

// SupervisorConfig installs protected observations and current host authority.
type SupervisorConfig struct {
	Registrations []SupervisorRegistration
	Repository    evidence.Repository
	Authorizer    verification.BindingAuthorizer
	Clock         verification.Clock
}

// EqualitySupervisor corroborates every declared grader check against protected
// expected JSON and authenticated candidate exports, retaining the original bytes.
type EqualitySupervisor struct {
	config        SupervisorConfig
	registrations map[string]SupervisorRegistration
}

// NewEqualitySupervisor freezes a bounded roster without accessing artifacts.
func NewEqualitySupervisor(c SupervisorConfig) (*EqualitySupervisor, error) {
	if missingGradingPort(c.Repository) || missingGradingPort(c.Authorizer) || missingGradingPort(c.Clock) || len(c.Registrations) == 0 || len(c.Registrations) > 256 {
		return nil, verification.ErrInvalid
	}
	body, err := json.Marshal(c.Registrations)
	if err != nil || len(body) > 16<<20 {
		return nil, verification.ErrInvalid
	}
	var registrations []SupervisorRegistration
	if json.Unmarshal(body, &registrations) != nil {
		return nil, verification.ErrInvalid
	}
	c.Registrations = nil
	s := &EqualitySupervisor{config: c, registrations: make(map[string]SupervisorRegistration)}
	ids := make(map[id.ID]bool)
	for _, r := range registrations {
		if r.Manifest.Validate() != nil || r.Manifest.SupervisorRevision != EqualitySupervisorRevision || len(r.Manifest.StateProbes) != 0 || r.AssessmentID.Prefix() != evidence.PrefixArtifact || ids[r.AssessmentID] {
			return nil, verification.ErrInvalid
		}
		if _, ok := s.registrations[r.Manifest.Digest]; ok {
			return nil, verification.ErrInvalid
		}
		for _, ref := range append([]evidence.Ref{r.Manifest.Binding.Candidate}, r.Manifest.Binding.Evidence...) {
			if ref.ID == r.AssessmentID {
				return nil, verification.ErrInvalid
			}
		}
		ids[r.AssessmentID] = true
		s.registrations[r.Manifest.Digest] = r
	}
	return s, nil
}

type equalityDeclaration struct {
	Protocol string   `json:"protocol"`
	Checks   []string `json:"checks"`
}
type equalityValues struct {
	Protocol string                     `json:"protocol"`
	Values   map[string]json.RawMessage `json:"values"`
}
type equalityVerdicts struct {
	Protocol string                         `json:"protocol"`
	Checks   map[string]verification.Status `json:"checks"`
}
type retainedAssessment struct {
	Protocol        string                         `json:"protocol"`
	ManifestDigest  string                         `json:"manifest_digest"`
	CandidateDigest string                         `json:"candidate_digest"`
	GraderDigest    string                         `json:"grader_digest"`
	Candidate       evidence.Ref                   `json:"candidate"`
	Grader          evidence.Ref                   `json:"grader"`
	Checks          []verification.SupervisedCheck `json:"checks"`
}

// Assess rechecks current authority and every artifact on retries. A changed,
// missing, revoked or expired observation cannot reuse a previous assessment.
func (s *EqualitySupervisor) Assess(ctx context.Context, o verification.SandboxObservation) (verification.SandboxAssessment, error) {
	if s == nil {
		return verification.SandboxAssessment{}, verification.ErrInvalid
	}
	r, ok := s.registrations[o.Manifest.Digest]
	if !ok || o.Manifest.Validate() != nil || o.Manifest.Digest != r.Manifest.Digest || o.Candidate.Validate() != nil || o.Grader.Validate() != nil || len(o.CandidateArtifacts) != 1 || len(o.GraderArtifacts) != 1 || o.Candidate.ValidateArtifacts(o.CandidateArtifacts, true) != nil || o.Grader.ValidateArtifacts(o.GraderArtifacts, true) != nil {
		return verification.SandboxAssessment{}, verification.ErrInvalid
	}
	m := r.Manifest
	if len(o.Grader.Inputs) != 4 || o.Grader.Inputs[3].Path != "/inputs/candidate-export" {
		return verification.SandboxAssessment{}, verification.ErrInvalid
	}
	exportDigest, exportErr := o.CandidateArtifacts[0].BindingDigest()
	inputDigest, inputErr := o.Grader.Inputs[3].Ref.BindingDigest()
	if exportErr != nil || inputErr != nil || exportDigest != inputDigest {
		return verification.SandboxAssessment{}, verification.ErrInvalid
	}
	template := o.Grader
	template.Inputs = append([]sandbox.Input(nil), template.Inputs[:3]...)
	var sealErr error
	template, sealErr = sandbox.Seal(template)
	if sealErr != nil || verification.ValidateSandboxTestPlan(verification.SandboxTestPlan{ManifestDigest: m.Digest, Candidate: o.Candidate, Grader: template}, m) != nil {
		return verification.SandboxAssessment{}, verification.ErrInvalid
	}
	for _, jobScope := range []cortex.Scope{o.Candidate.Scope, o.Grader.Scope} {
		if jobScope.Canonical() != m.Binding.Scope.Canonical() {
			return verification.SandboxAssessment{}, verification.ErrInvalid
		}
	}
	if o.Candidate.Principal != m.Binding.Context.PrincipalRef || o.Grader.Principal != m.Binding.Context.PrincipalRef {
		return verification.SandboxAssessment{}, verification.ErrInvalid
	}
	refs := []evidence.Ref{m.TestBundle, m.ResultSchema, m.FixtureDataset, o.CandidateArtifacts[0], o.GraderArtifacts[0]}
	bodies := make([][]byte, len(refs))
	for i, ref := range refs {
		if ref.ID == r.AssessmentID {
			return verification.SandboxAssessment{}, verification.ErrInvalid
		}
		body, err := s.read(ctx, m, ref)
		if err != nil {
			return verification.SandboxAssessment{}, err
		}
		bodies[i] = body
	}
	var tests, schema equalityDeclaration
	var expected, actual equalityValues
	var verdicts equalityVerdicts
	if strictJSON(bodies[0], &tests) != nil || strictJSON(bodies[1], &schema) != nil || strictJSON(bodies[2], &expected) != nil || strictJSON(bodies[3], &actual) != nil || strictJSON(bodies[4], &verdicts) != nil || tests.Protocol != EqualityProtocol || schema.Protocol != EqualityProtocol || expected.Protocol != EqualityProtocol || actual.Protocol != EqualityProtocol || verdicts.Protocol != EqualityProtocol || !reflect.DeepEqual(tests.Checks, m.RequiredChecks) || !reflect.DeepEqual(schema.Checks, m.RequiredChecks) || len(expected.Values) != len(m.RequiredChecks) || len(actual.Values) != len(m.RequiredChecks) || len(verdicts.Checks) != len(m.RequiredChecks) {
		return verification.SandboxAssessment{}, verification.ErrInvalid
	}
	a := verification.SandboxAssessment{ManifestDigest: m.Digest, SupervisorRevision: m.SupervisorRevision, CandidateDigest: o.Candidate.Digest, GraderDigest: o.Grader.Digest}
	for _, name := range m.RequiredChecks {
		want, wok := expected.Values[name]
		got, gok := actual.Values[name]
		claimed, cok := verdicts.Checks[name]
		if !wok || !gok || !cok {
			return verification.SandboxAssessment{}, verification.ErrInvalid
		}
		var x, y any
		if strictJSON(want, &x) != nil || strictJSON(got, &y) != nil {
			return verification.SandboxAssessment{}, verification.ErrInvalid
		}
		status := verification.StatusFailed
		if reflect.DeepEqual(x, y) {
			status = verification.StatusPassed
		}
		if claimed != status {
			return verification.SandboxAssessment{}, verification.ErrInvalid
		}
		a.Checks = append(a.Checks, verification.SupervisedCheck{Name: name, Status: status})
	}
	data, err := json.Marshal(retainedAssessment{Protocol: EqualityProtocol, ManifestDigest: m.Digest, CandidateDigest: o.Candidate.Digest, GraderDigest: o.Grader.Digest, Candidate: refs[3], Grader: refs[4], Checks: a.Checks})
	if err != nil {
		return verification.SandboxAssessment{}, verification.ErrInvalid
	}
	created, until := refs[3].CreatedAt, refs[3].RetainUntil
	for _, ref := range refs {
		if ref.CreatedAt.After(created) {
			created = ref.CreatedAt
		}
		if ref.RetainUntil.Before(until) {
			until = ref.RetainUntil
		}
	}
	artifact, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: r.AssessmentID, MediaType: "application/json", Scope: m.Binding.Scope, Version: m.SupervisorRevision, Producer: EqualityProtocol, Sensitivity: evidence.SensitivityRestricted, CreatedAt: created, RetainUntil: until}, Data: data})
	if err != nil {
		return verification.SandboxAssessment{}, err
	}
	if authErr := s.authorize(ctx, m); authErr != nil {
		return verification.SandboxAssessment{}, authErr
	}
	ref, err := s.config.Repository.Put(ctx, artifact)
	if err != nil {
		return verification.SandboxAssessment{}, err
	}
	wantDigest, wantErr := artifact.Ref.BindingDigest()
	gotDigest, digestErr := ref.BindingDigest()
	if wantErr != nil || digestErr != nil || wantDigest != gotDigest {
		return verification.SandboxAssessment{}, verification.ErrInvalid
	}
	// Read again after publication so a concurrent tombstone or grant change
	// cannot be hidden by a cached original observation.
	for _, input := range append(refs, ref) {
		if _, err = s.read(ctx, m, input); err != nil {
			return verification.SandboxAssessment{}, err
		}
	}
	a.ObservationRefs = []evidence.Ref{refs[3], refs[4], ref}
	return a, nil
}
func (s *EqualitySupervisor) authorize(ctx context.Context, m verification.EvaluationManifest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if cortex.ScopeFromContext(ctx).Canonical() != m.Binding.Scope.Canonical() {
		return verification.ErrInvalid
	}
	now := s.config.Clock.Now()
	for _, source := range m.Binding.Context.Sources {
		if source.RetrievedAt.After(now) || (m.Criterion.MaxSourceAge > 0 && now.Sub(source.RetrievedAt) > m.Criterion.MaxSourceAge) {
			return verification.ErrInvalid
		}
	}
	body, marshalErr := json.Marshal(m.Binding)
	if marshalErr != nil {
		return verification.ErrInvalid
	}
	var detached verification.Binding
	if json.Unmarshal(body, &detached) != nil {
		return verification.ErrInvalid
	}
	if err := s.config.Authorizer.AuthorizeVerification(ctx, detached); err != nil {
		return err
	}
	return ctx.Err()
}
func (s *EqualitySupervisor) read(ctx context.Context, m verification.EvaluationManifest, ref evidence.Ref) (data []byte, err error) {
	if ref.Validate() != nil || ref.Scope.Canonical() != m.Binding.Scope.Canonical() || ref.Bytes > 1<<20 || ref.Sensitivity != evidence.SensitivityRestricted || ref.MediaType != "application/json" || !freshRef(ref, s.config.Clock.Now()) {
		return nil, verification.ErrInvalid
	}
	if authErr := s.authorize(ctx, m); authErr != nil {
		return nil, authErr
	}
	reader, err := s.config.Repository.Open(ctx, ref)
	if reader != nil {
		defer func() { err = errors.Join(err, reader.Close()) }()
	}
	if err != nil {
		return nil, err
	}
	if reader == nil {
		return nil, verification.ErrInvalid
	}
	data, err = io.ReadAll(io.LimitReader(reader, ref.Bytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != ref.Bytes || evidence.Digest(data) != ref.SHA256 {
		return nil, evidence.ErrDigestMismatch
	}
	if !freshRef(ref, s.config.Clock.Now()) {
		return nil, verification.ErrInvalid
	}
	if authErr := s.authorize(ctx, m); authErr != nil {
		return nil, authErr
	}
	return data, nil
}
func freshRef(ref evidence.Ref, now time.Time) bool {
	return !ref.CreatedAt.After(now) && now.Before(ref.RetainUntil)
}

// strictJSON also rejects duplicate object names, which encoding/json otherwise
// accepts using the final value. Maximum depth and bytes bound hostile exports.
func strictJSON(body []byte, target any) error {
	if len(body) == 0 || len(body) > 1<<20 || !utf8.Valid(body) {
		return verification.ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	if scanJSON(d, 0) != nil {
		return verification.ErrInvalid
	}
	if _, err := d.Token(); err != io.EOF {
		return verification.ErrInvalid
	}
	d = json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	d.UseNumber()
	if d.Decode(target) != nil {
		return verification.ErrInvalid
	}
	return nil
}
func scanJSON(d *json.Decoder, depth int) error {
	if depth > 32 {
		return verification.ErrInvalid
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, keyErr := d.Token()
			name, nameOK := key.(string)
			if keyErr != nil || !nameOK || seen[name] {
				return verification.ErrInvalid
			}
			seen[name] = true
			if scanErr := scanJSON(d, depth+1); scanErr != nil {
				return scanErr
			}
		}
	case '[':
		for d.More() {
			if scanErr := scanJSON(d, depth+1); scanErr != nil {
				return scanErr
			}
		}
	default:
		return verification.ErrInvalid
	}
	closeToken, err := d.Token()
	if err != nil {
		return err
	}
	if (delimiter == '{' && closeToken != json.Delim('}')) || (delimiter == '[' && closeToken != json.Delim(']')) {
		return verification.ErrInvalid
	}
	return nil
}

func missingGradingPort(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}
