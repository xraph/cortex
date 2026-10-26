package engine

import "github.com/xraph/cortex/coordination"

// WithArtifactPublication installs bounded publication of accepted task outputs.
// Conflicts remain durable until an explicitly authorized ArtifactBoard resolution.
// The host resolves current source bindings and supplies publication authority.
func WithArtifactPublication(config coordination.ArtifactPublicationConfig) Option {
	return func(e *Engine) error {
		if e.artifactPublication != nil {
			return coordination.ErrInvalidScheduler
		}
		applied := config
		report := applied.ReportError
		applied.ReportError = func(err error) {
			e.logError("artifact publication failed", err)
			if report != nil {
				report(err)
			}
		}
		service, err := coordination.NewArtifactPublicationService(applied)
		if err != nil {
			return err
		}
		e.artifactPublication = service
		return nil
	}
}

// ArtifactPublicationStatus reports bounded publication counters and configuration.
func (e *Engine) ArtifactPublicationStatus() (coordination.TaskServiceStatus, bool) {
	if e.artifactPublication == nil {
		return coordination.TaskServiceStatus{}, false
	}
	return e.artifactPublication.Status(), true
}
