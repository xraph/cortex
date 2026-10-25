package storetest

import (
	"context"
	"testing"

	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

// PublishedLesson creates an accepted reflection with durable decision evidence
// and a pending outbox record. Adapters can reuse the storage conformance fixture.
func PublishedLesson(t *testing.T, factory Factory) (store.Store, context.Context, optimization.PublicationRequest) {
	t.Helper()
	return publishedLessonFixture(t, factory)
}
