package workerstore

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"testing"

	"github.com/xraph/cortex/evidence"
)

type captureProcessInput struct {
	Database  string
	PublicKey []byte
	Claim     Claim
	Ref       evidence.Ref
	Confirm   bool
}

func TestCaptureBindingSurvivesAbruptProcessExit(t *testing.T) {
	for _, confirm := range []bool{false, true} {
		t.Run(map[bool]string{false: "intent", true: "verified"}[confirm], func(t *testing.T) {
			s, cfg, claim, ref := captureFixture(t)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			payload, err := json.Marshal(captureProcessInput{Database: cfg.Database, PublicKey: cfg.PublicKey, Claim: claim, Ref: ref, Confirm: confirm})
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestCaptureProcessHelper$")
			cmd.Env = append(os.Environ(), "CORTEX_CAPTURE_PROCESS_HELPER=1")
			cmd.Stdin = bytes.NewReader(payload)
			if output, runErr := cmd.CombinedOutput(); runErr != nil {
				t.Fatalf("capture process: %s %v", output, runErr)
			}
			reopened := openStore(t, cfg)
			records, err := reopened.Captures(t.Context(), claim.Record.Receipt.Job.ID)
			want := CaptureIntent
			if confirm {
				want = CaptureVerified
			}
			if err != nil || len(records) != 1 || records[0].State != want || !sameCaptureRef(records[0].Ref, ref) {
				t.Fatal("process exit lost capture", records, err)
			}
			if _, err = reopened.VerifyClaim(t.Context(), claim.Record.Receipt.Job.ID, claim.Token, claim.Record.Revision); err != nil {
				t.Fatal("capture changed claim", err)
			}
		})
	}
}

func TestCaptureProcessHelper(t *testing.T) {
	if os.Getenv("CORTEX_CAPTURE_PROCESS_HELPER") != "1" {
		return
	}
	var input captureProcessInput
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 128<<10)).Decode(&input); err != nil {
		t.Fatal(err)
	}
	s, err := Open(t.Context(), Config{Database: input.Database, Broker: "trusted", PublicKey: input.PublicKey, MaxJobs: 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.RecordCapture(t.Context(), input.Claim.Record.Receipt.Job.ID, input.Claim.Token, input.Claim.Record.Revision, input.Ref); err != nil {
		t.Fatal(err)
	}
	if input.Confirm {
		if _, err = s.ConfirmCapture(t.Context(), input.Claim.Record.Receipt.Job.ID, input.Claim.Token, input.Claim.Record.Revision, input.Ref); err != nil {
			t.Fatal(err)
		}
	}
	// Leave SQLite and every Go defer untouched after the committed transition.
	os.Exit(0)
}
