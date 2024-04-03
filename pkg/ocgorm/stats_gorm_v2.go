//go:build go1.11
// +build go1.11

package ocgorm

import (
	"context"
	"strings"
	"sync"
	"time"

	"go.opencensus.io/stats"
	gormv2 "gorm.io/gorm"
)

// RecordStatsV2 records database statistics for provided sql.DB at the provided
// interval. You should defer execution of this function after you establish
// connection to the database `if err == nil { ocgorm.RecordStats(db, 5*time.Second); }
func RecordStatsV2(dbv2 *gormv2.DB, interval time.Duration) (fnStop func()) {
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
				sqlDB, err := dbv2.DB()
				if err != nil {
					return
				}
				dbStats := sqlDB.Stats()

				if dbStats.OpenConnections == 0 { // We cleanup the ticker in the event that the database is unavailable
					if err := sqlDB.Ping(); err != nil && strings.Contains(err.Error(), "database is closed") {
						ticker.Stop()
						return
					}
				}

				stats.Record(ctx,
					MeasureOpenConnections.M(int64(dbStats.OpenConnections)),
					MeasureIdleConnections.M(int64(dbStats.Idle)),
					MeasureActiveConnections.M(int64(dbStats.InUse)),
					MeasureWaitCount.M(dbStats.WaitCount),
					MeasureWaitDuration.M(float64(dbStats.WaitDuration.Nanoseconds())/1e6),
					MeasureIdleClosed.M(dbStats.MaxIdleClosed),
					MeasureLifetimeClosed.M(dbStats.MaxLifetimeClosed),
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
