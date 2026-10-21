//go:build linux || darwin

package service

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"

	"golang.org/x/sys/unix"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/containment"
	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerstore"
)

const maxCredentialBytes = 1 << 20

// Credential is a private worker capability. Never log its body or token, copy
// it into guest mounts, or use it to authorize a second service start. The worker
// must confirm current journal ownership through authenticated daemon control.
type Credential struct {
	SchemaVersion int                `json:"schema_version"`
	ProfileDigest string             `json:"profile_digest"`
	Record        workerstore.Record `json:"record"`
	Token         string             `json:"token"`
}

func (c Credential) validate() error {
	if c.SchemaVersion != 1 || !evidence.ValidDigest(c.ProfileDigest) || c.Record.SchemaVersion != 1 || c.Record.Phase != workerstore.Claimed || c.Record.Revision < 2 || c.Record.CancelRequested || c.Record.Result != nil || !evidence.ValidDigest(c.Record.TokenHash) || len(c.Token) != 64 {
		return fmt.Errorf("cortex: invalid launch credential")
	}
	token, err := hex.DecodeString(c.Token)
	if err != nil || len(token) != 32 || hex.EncodeToString(token) != c.Token || subtle.ConstantTimeCompare([]byte(evidence.Digest([]byte(c.Token))), []byte(c.Record.TokenHash)) != 1 {
		return fmt.Errorf("cortex: launch credential token mismatch")
	}
	return c.Record.Receipt.Job.Validate()
}

func writeCredential(ctx context.Context, p LaunchProfile, claim workerstore.Claim, owner uint32) error {
	digest, err := p.Digest()
	if err != nil {
		return err
	}
	credential := Credential{SchemaVersion: 1, ProfileDigest: digest, Record: claim.Record, Token: claim.Token}
	if operationErr := credential.validate(); operationErr != nil {
		return operationErr
	}
	body, err := json.Marshal(credential)
	if err != nil {
		return err
	}
	if len(body) > maxCredentialBytes {
		return fmt.Errorf("cortex: launch credential exceeds one MiB")
	}
	if operationErr := ctx.Err(); operationErr != nil {
		return operationErr
	}
	root, err := hostfs.OpenDirectory(p.StateDirectory, owner)
	if err != nil {
		return err
	}
	defer root.Close()
	info, err := root.Stat()
	if err != nil {
		return err
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("cortex: unsafe launch credential root")
	}
	launches, err := privateDirectory(root, "launch", owner, false)
	if err != nil {
		return err
	}
	defer launches.Close()
	job, err := privateDirectory(launches, claim.Record.Receipt.Job.ID.String(), owner, true)
	if err != nil {
		return err
	}
	defer job.Close()
	fd, err := unix.Openat(int(job.Fd()), "claim.tmp", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o400)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), "claim.tmp")
	written, writeErr := file.Write(body)
	if writeErr == nil && written != len(body) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if operationErr := errors.Join(writeErr, file.Close(), ctx.Err()); operationErr != nil {
		return operationErr
	}
	if operationErr := unix.Renameat(int(job.Fd()), "claim.tmp", int(job.Fd()), "claim.json"); operationErr != nil {
		return operationErr
	}
	return errors.Join(job.Sync(), launches.Sync(), root.Sync(), ctx.Err())
}

func privateDirectory(parent *os.File, name string, owner uint32, exclusive bool) (*os.File, error) {
	err := unix.Mkdirat(int(parent.Fd()), name, 0o700)
	if err != nil && (exclusive || !errors.Is(err, unix.EEXIST)) {
		return nil, err
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	directory := os.NewFile(uintptr(fd), name)
	var stat unix.Stat_t
	if err = unix.Fstat(fd, &stat); err != nil {
		return nil, errors.Join(err, directory.Close())
	}
	if stat.Uid != owner || stat.Mode&0o7777 != 0o700 {
		return nil, errors.Join(fmt.Errorf("cortex: unsafe credential directory"), directory.Close())
	}
	return directory, nil
}

// ReadCredential verifies the systemd credential copy in the exact worker
// subgroup. This verifies its signed acceptance and deployment binding, not
// current journal ownership or fresh user authorization. Those checks remain
// mandatory immediately before guest execution.
func ReadCredential(ctx context.Context, directory string, key id.ID, profileDigest, brokerName string, publicKey ed25519.PublicKey) (Credential, error) {
	if runtime.GOOS != "linux" {
		return Credential{}, errUnsupported
	}
	if err := containment.VerifyCurrent(ctx, key); err != nil {
		return Credential{}, err
	}
	return readCredential(ctx, directory, "cortex-claim", key, profileDigest, brokerName, publicKey, 0, time.Now())
}

func readCredential(ctx context.Context, directory, name string, key id.ID, profileDigest, brokerName string, publicKey ed25519.PublicKey, owner uint32, now time.Time) (Credential, error) {
	if _, err := containment.ForJob(key); err != nil {
		return Credential{}, err
	}
	if name != "claim.json" && name != "cortex-claim" {
		return Credential{}, fmt.Errorf("cortex: invalid credential name")
	}
	if !evidence.ValidDigest(profileDigest) || brokerName == "" || len(publicKey) != ed25519.PublicKeySize || now.IsZero() {
		return Credential{}, fmt.Errorf("cortex: incomplete credential verification")
	}
	if err := ctx.Err(); err != nil {
		return Credential{}, err
	}
	root, err := hostfs.OpenDirectory(directory, owner)
	if err != nil {
		return Credential{}, err
	}
	defer root.Close()
	fd, err := unix.Openat(int(root.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return Credential{}, err
	}
	file := os.NewFile(uintptr(fd), name)
	var stat unix.Stat_t
	if err = unix.Fstat(fd, &stat); err != nil {
		return Credential{}, errors.Join(err, file.Close())
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != owner || stat.Nlink != 1 || stat.Mode&0o7777 != 0o400 || stat.Size <= 0 || stat.Size > maxCredentialBytes {
		return Credential{}, errors.Join(fmt.Errorf("cortex: unsafe credential file"), file.Close())
	}
	body, readErr := io.ReadAll(io.LimitReader(file, maxCredentialBytes+1))
	if operationErr := errors.Join(readErr, file.Close(), ctx.Err()); operationErr != nil {
		return Credential{}, operationErr
	}
	if len(body) > maxCredentialBytes {
		return Credential{}, fmt.Errorf("cortex: oversized credential")
	}
	var credential Credential
	if err = json.Unmarshal(body, &credential); err != nil {
		return Credential{}, fmt.Errorf("cortex: malformed launch credential")
	}
	canonical, err := json.Marshal(credential)
	if err != nil || !bytes.Equal(body, canonical) {
		return Credential{}, fmt.Errorf("cortex: noncanonical launch credential")
	}
	if operationErr := credential.validate(); operationErr != nil {
		return Credential{}, operationErr
	}
	receipt := credential.Record.Receipt
	if receipt.Job.ID != key || credential.ProfileDigest != profileDigest || receipt.Broker != brokerName {
		return Credential{}, broker.ErrDenied
	}
	if operationErr := broker.VerifyReceipt(receipt, publicKey); operationErr != nil {
		return Credential{}, operationErr
	}
	if operationErr := checkAcceptanceTime(receipt, now); operationErr != nil {
		return Credential{}, operationErr
	}
	if operationErr := receipt.Capabilities.Supports(receipt.Job); operationErr != nil {
		return Credential{}, operationErr
	}
	if err := ctx.Err(); err != nil {
		return Credential{}, err
	}
	return credential, nil
}
