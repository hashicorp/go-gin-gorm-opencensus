package ocgormv2

import (
	"context"

	"gorm.io/gorm"
)

var (
	contextScopeKey = "_opencensusContext"
)

// WithContext sets the current context in the db instance for instrumentation.
func WithContext(ctx context.Context, db *gorm.DB) *gorm.DB {
	// we MUST call `Session` here because otherwise resulting *gorm.DB will be unsafe
	// to reuse, see https://gorm.io/docs/method_chaining.html for more information
	return db.Set(contextScopeKey, ctx).Session(&gorm.Session{})
}
