package assurance

import (
	"context"
	"encoding/json"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/verification"
)

type resultMaterial struct {
	contract verification.Contract
	reports  []verification.Report
	digests  []string
	output   string
}

// loadResultMaterial shares bounded evidence loading between initial commit and
// later reads. Callers validate the entire snapshot at trusted observation time.
func (s *Repository) loadResultMaterial(ctx context.Context, req verification.CommitRequest) (resultMaterial, error) {
	contract, err := s.GetContract(ctx, req.ContractID)
	if err != nil {
		return resultMaterial{}, err
	}
	if contract.Digest != req.Current.ContractDigest {
		return resultMaterial{}, verification.ErrConflict
	}
	reports := make([]verification.Report, 0, len(req.ReportIDs))
	digests := make([]string, 0, len(req.ReportIDs))
	// Bound total material processed while locks are held, in addition to each
	// record's one MiB limit. Artifact bytes are released between loads.
	total := 0
	for _, reportID := range req.ReportIDs {
		report, readErr := s.GetReport(ctx, reportID)
		if readErr != nil {
			return resultMaterial{}, readErr
		}
		body, marshalErr := json.Marshal(report)
		if marshalErr != nil {
			return resultMaterial{}, marshalErr
		}
		total += len(body)
		if total > 8<<20 {
			return resultMaterial{}, verification.ErrInvalid
		}
		reports = append(reports, report)
		digests = append(digests, report.Digest)
	}
	var candidateOutput string
	for index, ref := range append([]evidence.Ref{req.Current.Candidate}, req.Current.Evidence...) {
		artifact, readErr := s.LoadEvidence(ctx, ref)
		if readErr != nil {
			return resultMaterial{}, readErr
		}
		total += len(artifact.Data)
		if total > 8<<20 {
			return resultMaterial{}, verification.ErrInvalid
		}
		if index == 0 {
			candidateOutput = string(artifact.Data)
		}
	}
	return resultMaterial{contract: contract, reports: reports, digests: digests, output: candidateOutput}, nil
}

func (m resultMaterial) snapshot(receipt verification.CommitReceipt, at time.Time) verification.ResultSnapshot {
	return verification.ResultSnapshot{SchemaVersion: 1, Receipt: receipt, Contract: m.contract, Reports: m.reports, Output: m.output, ObservedAt: at}
}
