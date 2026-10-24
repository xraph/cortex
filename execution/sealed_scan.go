package execution

import "context"

func authorizeSealedScan(ctx context.Context, storage RunCallSealStore, seal RunCallSeal, authorize func(context.Context, RunCallSeal) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	offered := seal
	offered.Request = cloneSealRequest(seal.Request)
	if err := authorize(ctx, offered); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := storage.GetRunCallSeal(ctx, seal.Request.RunID)
	if err != nil {
		return err
	}
	if current == nil || current.Validate() != nil || current.RequestDigest != seal.RequestDigest || !current.At.Equal(seal.At) {
		return ErrRunCallConflict
	}
	return ctx.Err()
}
