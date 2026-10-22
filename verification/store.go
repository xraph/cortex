package verification

import (
	"context"

	"github.com/xraph/cortex/id"
)

// ListRequest identifies a single run in the exact caller scope. Limit must be
// 1..256. After is an exclusive report ID cursor, ordered lexicographically.
type ListRequest struct {
	RunID id.AgentRunID
	After id.ID
	Limit int
}

// Store is an optional capability for immutable contracts and reports. Reads
// and writes require exact context scope, never inherited scope. Reusing an ID
// is idempotent only for identical content. Report writes validate the saved
// contract and its digest. A report's digest is integrity, not evaluator trust;
// only authenticated host verifier paths may call SaveReport.
type Store interface {
	CreateContract(context.Context, Contract) error
	GetContract(context.Context, id.ID) (Contract, error)
	SaveReport(context.Context, Report) error
	GetReport(context.Context, id.ID) (Report, error)
	ListReports(context.Context, ListRequest) ([]Report, error)
}
