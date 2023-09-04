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
	return db.Set(contextScopeKey, ctx)
}
