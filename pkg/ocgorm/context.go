package ocgorm

import (
	"context"

	"github.com/jinzhu/gorm"
)

// WithContextV2 sets the current context in the db instance for instrumentation.
func WithContextV2(ctx context.Context, db *gorm.DB) *gorm.DB {
	return db.New().Set(contextScopeKey, ctx)
}
