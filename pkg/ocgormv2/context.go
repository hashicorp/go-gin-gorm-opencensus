package ocgormv2

import (
	"context"

	"gorm.io/gorm"
)

// Gorm scope key
var (
	contextScopeKey = "_opencensusContext"


// WithContext sets the current context in the db instance for instrumentation.
func WithContext(ctx context.Context, db *gorm.DB) *gorm.DB {
	return db.New().Set(contextScopeKey, ctx)
}
