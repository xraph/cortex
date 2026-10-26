package engine

import "github.com/xraph/cortex/suspension"

// NewResumeCloser uses durable engine storage to close uncertain resumed runs
// as incomplete. The host must authenticate permanent worker/tool quiescence
// and hold current authority. This API grants no retry or successful completion.
// Durable terminal events are atomic; background recovery skips terminal runs.
// Claims, effects and budget settlement stay separate.
func (e *Engine) NewResumeCloser(fence suspension.ResumeClosureFence, authorizer suspension.ResumeClosureAuthorizer) (suspension.ResumeCloser, error) {
	if e == nil {
		return nil, suspension.ErrResumeAdmissionUnavailable
	}
	storage, ok := e.store.(suspension.ResumeClosureStore)
	if !ok {
		return nil, suspension.ErrResumeAdmissionUnavailable
	}
	return suspension.NewResumeCloser(storage, fence, authorizer)
}
