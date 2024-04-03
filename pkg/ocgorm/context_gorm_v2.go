package ocgorm

import (
	"context"

	"gorm.io/gorm"
)

// WithContext sets the current context in the db instance for instrumentation.
// Deprecated: prefer direct usage of db.WithContext(ctx).
func WithContext(ctx context.Context, db *gorm.DB) *gorm.DB {
	return db.WithContext(ctx)
}
