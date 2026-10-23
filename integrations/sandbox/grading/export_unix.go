//go:build linux || darwin

package grading

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
	"github.com/xraph/cortex/verification"
)

// FileExportDestination maps a logical target and revision to a private directory.
// The host provisions and protects this directory; request data supplies no paths.
type FileExportDestination struct {
	Name      string
	Revision  string
	Directory string
}

// FileExportConfig installs a local export command with a global operation journal.
// Command.Sink must be nil. Directories must be distinct, private and owned by Owner.
type FileExportConfig struct {
	Command          verification.ArtifactExporterConfig
	JournalDirectory string
	Destinations     []FileExportDestination
	Owner            uint32
}

// NewFileExporter binds protected, non-replacing file publication to the guarded
// root export command. It creates no directories, files, listeners or workers.
func NewFileExporter(c FileExportConfig) (*verification.ArtifactExporter, error) {
	sink, err := newFileExportSink(c)
	if err != nil {
		return nil, err
	}
	c.Command.Sink = sink
	return verification.NewArtifactExporter(c.Command)
}

type exportDirectory struct {
	path     string
	identity os.FileInfo
}
type exportTarget struct {
	revision  string
	directory exportDirectory
}
type fileExportSink struct {
	owner      uint32
	journal    exportDirectory
	targets    map[string]exportTarget
	authorizer verification.ArtifactExportAuthorizer
}

func newFileExportSink(c FileExportConfig) (*fileExportSink, error) {
	if c.Command.Sink != nil || c.Command.Authorizer == nil || len(c.Destinations) == 0 || len(c.Destinations) > 256 {
		return nil, verification.ErrInvalid
	}
	journal, err := pinExportDirectory(c.JournalDirectory, c.Owner)
	if err != nil {
		return nil, err
	}
	sink := &fileExportSink{owner: c.Owner, journal: journal, targets: make(map[string]exportTarget), authorizer: c.Command.Authorizer}
	directories := []exportDirectory{journal}
	for _, destination := range c.Destinations {
		if !fileExportName(destination.Name, 512) || !fileExportName(destination.Revision, 256) {
			return nil, verification.ErrInvalid
		}
		if _, exists := sink.targets[destination.Name]; exists {
			return nil, verification.ErrInvalid
		}
		directory, pinErr := pinExportDirectory(destination.Directory, c.Owner)
		if pinErr != nil {
			return nil, pinErr
		}
		for _, prior := range directories {
			if os.SameFile(directory.identity, prior.identity) || strings.HasPrefix(directory.path, prior.path+string(filepath.Separator)) || strings.HasPrefix(prior.path, directory.path+string(filepath.Separator)) {
				return nil, verification.ErrInvalid
			}
		}
		directories = append(directories, directory)
		sink.targets[destination.Name] = exportTarget{revision: destination.Revision, directory: directory}
	}
	return sink, nil
}
func fileExportName(s string, maximum int) bool {
	return s != "" && len(s) <= maximum && strings.TrimSpace(s) == s && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}
func pinExportDirectory(path string, owner uint32) (exportDirectory, error) {
	directory, err := hostfs.OpenDirectory(path, owner)
	if err != nil {
		return exportDirectory{}, err
	}
	info, statErr := directory.Stat()
	if closeErr := errors.Join(statErr, directory.Close()); closeErr != nil {
		return exportDirectory{}, closeErr
	}
	if info.Mode().Perm() != 0o700 || info.Mode()&(os.ModeSticky|os.ModeSetgid|os.ModeSetuid) != 0 {
		return exportDirectory{}, verification.ErrInvalid
	}
	return exportDirectory{path: path, identity: info}, nil
}
func (d exportDirectory) check(owner uint32) error {
	current, err := pinExportDirectory(d.path, owner)
	if err != nil {
		return err
	}
	if !os.SameFile(current.identity, d.identity) {
		return verification.ErrConflict
	}
	return nil
}

func (s *fileExportSink) ExportArtifact(ctx context.Context, request verification.ArtifactExportRequest, data []byte) (verification.ArtifactExportReceipt, error) {
	r, digest, err := request.Freeze()
	if err != nil {
		return verification.ArtifactExportReceipt{}, err
	}
	if cortex.ScopeFromContext(ctx).Canonical() != r.Artifact.Scope.Canonical() || int64(len(data)) != r.Artifact.Bytes || evidence.Digest(data) != r.Artifact.SHA256 {
		return verification.ArtifactExportReceipt{}, verification.ErrInvalid
	}
	destination, ok := s.targets[r.Destination]
	if !ok || destination.revision != r.DestinationRevision {
		return verification.ArtifactExportReceipt{}, verification.ErrResultAccessDenied
	}
	check := func() error {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		if directoryErr := s.journal.check(s.owner); directoryErr != nil {
			return directoryErr
		}
		if directoryErr := destination.directory.check(s.owner); directoryErr != nil {
			return directoryErr
		}
		detached, _, freezeErr := r.Freeze()
		if freezeErr != nil {
			return freezeErr
		}
		if policyErr := s.authorizer.AuthorizeArtifactExport(ctx, detached); policyErr != nil {
			return policyErr
		}
		return ctx.Err()
	}
	if checkErr := check(); checkErr != nil {
		return verification.ArtifactExportReceipt{}, checkErr
	}
	body, err := json.Marshal(r)
	if err != nil {
		return verification.ArtifactExportReceipt{}, verification.ErrInvalid
	}
	reservation := hostfs.Pin{Path: filepath.Join(s.journal.path, r.ID.String()+".json"), SHA256: evidence.Digest(body), Bytes: int64(len(body)), Mode: 0o400}
	if publishErr := hostfs.PublishPin(ctx, reservation, s.owner, body, check); publishErr != nil {
		return verification.ArtifactExportReceipt{}, publishErr
	}
	artifact := hostfs.Pin{Path: filepath.Join(destination.directory.path, r.ID.String()), SHA256: r.Artifact.SHA256, Bytes: r.Artifact.Bytes, Mode: 0o400}
	if publishErr := hostfs.PublishPin(ctx, artifact, s.owner, data, check); publishErr != nil {
		return verification.ArtifactExportReceipt{}, publishErr
	}
	if checkErr := check(); checkErr != nil {
		return verification.ArtifactExportReceipt{}, checkErr
	}
	if verifyErr := errors.Join(hostfs.VerifyPin(ctx, reservation, s.owner), hostfs.VerifyPin(ctx, artifact, s.owner), ctx.Err()); verifyErr != nil {
		return verification.ArtifactExportReceipt{}, verifyErr
	}
	return verification.ArtifactExportReceipt{SchemaVersion: 1, ID: r.ID, RequestDigest: digest, SHA256: r.Artifact.SHA256, Bytes: r.Artifact.Bytes}, nil
}
