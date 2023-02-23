//go:build go1.11
// +build go1.11

package ocgormv2

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-gin-gorm-opencensus/pkg/ocgorm"
	"go.opencensus.io/stats"
	"go.opencensus.io/tag"
	"gorm.io/gorm"
)

// RecordStats records database statistics for provided sql.DB at the provided
// interval. You should defer execution of this function after you establish
// connection to the database `if err == nil { ocgorm.RecordStats(db, 5*time.Second); }
func RecordStats(db *gorm.DB, interval time.Duration, tags ...tag.Mutator) (fnStop func()) {
	var (
		closeOnce sync.Once
		ctx       = context.Background()
		ticker    = time.NewTicker(interval)
		done      = make(chan struct{})
	)

	go func() {
		for {
			select {
			case <-ticker.C:
				sqlDB, err := db.DB()
				if err != nil {
					continue
				}

				dbStats := sqlDB.Stats()

				if dbStats.OpenConnections == 0 { // We cleanup the ticker in the event that the database is unavailable
					if err := sqlDB.Ping(); strings.Contains(err.Error(), "database is closed") {
						ticker.Stop()
						return
					}
				}

				stats.RecordWithTags(ctx,
					tags,
					ocgorm.MeasureOpenConnections.M(int64(dbStats.OpenConnections)),
					ocgorm.MeasureIdleConnections.M(int64(dbStats.Idle)),
					ocgorm.MeasureActiveConnections.M(int64(dbStats.InUse)),
					ocgorm.MeasureWaitCount.M(dbStats.WaitCount),
					ocgorm.MeasureWaitDuration.M(float64(dbStats.WaitDuration.Nanoseconds())/1e6),
					ocgorm.MeasureIdleClosed.M(dbStats.MaxIdleClosed),
					ocgorm.MeasureLifetimeClosed.M(dbStats.MaxLifetimeClosed),
				)
			case <-done:
				ticker.Stop()
				return
			}
		}
	}()

	return func() {
		closeOnce.Do(func() {
			close(done)
		})
	}
}
