package operator

import (
	"context"

	opt "github.com/xraph/cortex/optimization"
)

// CatalogInput filters approved creation references without requesting snapshots.
type CatalogInput struct {
	PageInput
	Kind opt.CatalogKind `json:"kind,omitempty" query:"kind" optional:"true"`
}

// Catalog reads currently authorized host choices using a bound continuation.
func (r Reader) Catalog(ctx context.Context, in CatalogInput) (opt.ResourcePage[opt.CatalogChoice], error) {
	return read(r, func(o *opt.Optimizer) (opt.ResourcePage[opt.CatalogChoice], error) {
		return o.ListCatalog(ctx, opt.CatalogQuery{PageQuery: in.query(), Kind: in.Kind})
	})
}
