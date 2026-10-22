package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/sandbox"
)

func TestClaimCredentialIsPrivateBoundAndNeverReplaced(t *testing.T) {
	store, receipt, cfg := recoveryStore(t)
	p := profileFixture(t)
	claim, err := store.Claim(t.Context(), receipt.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = writeCredential(t.Context(), p, claim, uint32(os.Geteuid())); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(p.StateDirectory, "launch", receipt.Job.ID.String())
	info, err := os.Stat(filepath.Join(directory, "claim.json"))
	if err != nil || info.Mode().Perm() != 0o400 {
		t.Fatal("claim file is not private")
	}
	digest, err := p.Digest()
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := readCredential(t.Context(), directory, "claim.json", receipt.Job.ID, digest, cfg.Broker, cfg.PublicKey, uint32(os.Geteuid()), time.Now())
	if err != nil || loaded.Token != claim.Token || loaded.Record.Revision != claim.Record.Revision {
		t.Fatal("credential binding failed", err)
	}
	if err = writeCredential(t.Context(), p, claim, uint32(os.Geteuid())); err == nil {
		t.Fatal("existing claim directory was reused")
	}
	for _, key := range []id.ID{id.New(sandbox.PrefixJob), id.New("agt")} {
		if _, err = readCredential(t.Context(), directory, "claim.json", key, digest, cfg.Broker, cfg.PublicKey, uint32(os.Geteuid()), time.Now()); err == nil {
			t.Fatal("foreign credential accepted")
		}
	}
}

func TestCredentialRejectsTamperingExpiryAndAlias(t *testing.T) {
	for _, mode := range []string{"token", "profile", "receipt", "duplicate", "expired", "symlink", "permissions", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			store, receipt, cfg := recoveryStore(t)
			p := profileFixture(t)
			claim, err := store.Claim(t.Context(), receipt.Job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err = writeCredential(t.Context(), p, claim, uint32(os.Geteuid())); err != nil {
				t.Fatal(err)
			}
			directory := filepath.Join(p.StateDirectory, "launch", receipt.Job.ID.String())
			filename := filepath.Join(directory, "claim.json")
			body, err := os.ReadFile(filename)
			if err != nil {
				t.Fatal(err)
			}
			digest, _ := p.Digest()
			now := time.Now()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch mode {
			case "token":
				body = []byte(strings.Replace(string(body), claim.Token, strings.Repeat("0", 64), 1))
			case "profile":
				digest = strings.Repeat("0", 64)
			case "receipt":
				body = []byte(strings.Replace(string(body), "alice", "other", 1))
			case "duplicate":
				body = append([]byte(`{"schema_version":1,`), body[1:]...)
			case "expired":
				now = receipt.AcceptedAt.Add(receipt.Job.Limits.WallTime)
			case "symlink":
				if err = os.Rename(filename, filename+".real"); err != nil {
					t.Fatal(err)
				}
				if err = os.Symlink("claim.json.real", filename); err != nil {
					t.Fatal(err)
				}
			case "permissions":
				if err = os.Chmod(filename, 0o644); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				cancel()
			}
			if mode == "token" || mode == "receipt" || mode == "duplicate" {
				if err = os.Chmod(filename, 0o600); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filename, body, 0o600); err != nil {
					t.Fatal(err)
				}
				if err = os.Chmod(filename, 0o400); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = readCredential(ctx, directory, "claim.json", receipt.Job.ID, digest, cfg.Broker, cfg.PublicKey, uint32(os.Geteuid()), now); err == nil {
				t.Fatal("unsafe credential accepted")
			}
		})
	}
}

func TestCredentialPublicationRefusesAliasedOrPublicDirectory(t *testing.T) {
	for _, alias := range []bool{false, true} {
		store, receipt, _ := recoveryStore(t)
		p := profileFixture(t)
		claim, err := store.Claim(t.Context(), receipt.Job.ID)
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(p.StateDirectory, "launch")
		if alias {
			outside := t.TempDir()
			if err = os.Symlink(outside, target); err != nil {
				t.Fatal(err)
			}
		} else {
			if err = os.Mkdir(target, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err = writeCredential(t.Context(), p, claim, uint32(os.Geteuid())); err == nil {
			t.Fatal("unsafe launch directory accepted")
		}
		if _, err = os.Lstat(filepath.Join(target, receipt.Job.ID.String())); !os.IsNotExist(err) {
			t.Fatal("credential written through unsafe directory", err)
		}
	}
}
