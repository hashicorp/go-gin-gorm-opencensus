package ocgorm

import (
	"context"
	"errors"
	"fmt"
	"time"

	gormv1 "github.com/jinzhu/gorm"
	"go.opencensus.io/stats"
	"go.opencensus.io/tag"
	"go.opencensus.io/trace"
	gormv2 "gorm.io/gorm"
)

// RegisterCallbacksV2 registers the necessary callbacks in Gorm's hook system for instrumentation.
func RegisterCallbacksV2(dbv1 *gormv1.DB, dbv2 *gormv2.DB, opts ...Option) error {
	if dbv1 != nil {
		RegisterCallbacks(dbv1, opts...)
		return nil
	}

	c := &callbacks{
		defaultAttributes: []trace.Attribute{},
	}

	for _, opt := range opts {
		opt.apply(c)
	}

	return errors.Join(
		dbv2.Callback().Create().Before("gorm:create").Register("instrumentation:before_create", c.beforeCreateV2),
		dbv2.Callback().Create().After("gorm:create").Register("instrumentation:after_create", c.afterCreateV2),
		dbv2.Callback().Query().Before("gorm:query").Register("instrumentation:before_query", c.beforeQueryV2),
		dbv2.Callback().Query().After("gorm:query").Register("instrumentation:after_query", c.afterQueryV2),
		dbv2.Callback().Query().Before("gorm:row_query").Register("instrumentation:before_row_query", c.beforeRowQueryV2),
		dbv2.Callback().Query().After("gorm:row_query").Register("instrumentation:after_row_query", c.afterRowQueryV2),
		dbv2.Callback().Update().Before("gorm:update").Register("instrumentation:before_update", c.beforeUpdateV2),
		dbv2.Callback().Update().After("gorm:update").Register("instrumentation:after_update", c.afterUpdateV2),
		dbv2.Callback().Delete().Before("gorm:delete").Register("instrumentation:before_delete", c.beforeDeleteV2),
		dbv2.Callback().Delete().After("gorm:delete").Register("instrumentation:after_delete", c.afterDeleteV2))
}

func (c *callbacks) beforeV2(db *gormv2.DB, operation string) {
	ctx := db.Statement.Context
	if ctx == nil {
		ctx = context.Background()
	}

	ctx = c.startTraceV2(ctx, db, operation)
	ctx = c.startStatsV2(ctx, db, operation)

	db.Statement.Context = ctx
}

func (c *callbacks) afterV2(db *gormv2.DB) {
	c.endTraceV2(db)
	c.endStatsV2(db)
}

func (c *callbacks) startTraceV2(ctx context.Context, db *gormv2.DB, operation string) context.Context {
	// Context is missing, but we allow root spans to be created
	if ctx == nil {
		ctx = context.Background()
	}

	parentSpan := trace.FromContext(ctx)
	if parentSpan == nil && !c.allowRoot {
		return ctx
	}

	var span *trace.Span

	if parentSpan == nil {
		ctx, span = trace.StartSpan(
			context.Background(),
			fmt.Sprintf("gormv2:%s", operation),
			trace.WithSpanKind(trace.SpanKindClient),
			trace.WithSampler(c.startOptions.Sampler),
		)
	} else {
		ctx, span = trace.StartSpan(ctx, fmt.Sprintf("gormv2:%s", operation))
	}

	attributes := append(
		c.defaultAttributes,
		trace.StringAttribute(TableAttribute, db.Statement.Table),
	)

	if c.query {
		attributes = append(attributes, trace.StringAttribute(ResourceNameAttribute, db.Statement.SQL.String()))
	}

	span.AddAttributes(attributes...)

	return ctx
}

func (c *callbacks) endTraceV2(db *gormv2.DB) {
	span := trace.FromContext(db.Statement.Context)

	// Add query to the span if requested
	if c.query {
		span.AddAttributes(trace.StringAttribute(ResourceNameAttribute, db.Statement.SQL.String()))
	}

	var status trace.Status

	if db.Error != nil {
		if errors.Is(db.Error, gormv2.ErrRecordNotFound) {
			status.Code = trace.StatusCodeNotFound
		} else {
			status.Code = trace.StatusCodeUnknown
		}

		status.Message = db.Error.Error()
	}

	span.SetStatus(status)

	span.End()
}

func (c *callbacks) startStatsV2(ctx context.Context, db *gormv2.DB, operation string) context.Context {
	ctx, _ = tag.New(ctx,
		tag.Upsert(Operation, operation),
		tag.Upsert(Table, db.Statement.Table),
		tag.Upsert(queryStartPropagator, time.Now().UTC().Format(time.RFC3339Nano)),
	)

	return ctx
}

func (c *callbacks) endStatsV2(db *gormv2.DB) {
	if db.Error != nil {
		return
	}

	ctx := db.Statement.Context
	if ctx == nil {
		return
	}

	tags := tag.FromContext(ctx)
	ctx, _ = tag.New(ctx, tag.Delete(queryStartPropagator))
	queryStartNS, exists := tags.Value(queryStartPropagator)

	if exists {
		queryStart, err := time.Parse(time.RFC3339Nano, queryStartNS)
		if err != nil {
			return
		}

		timeSpentMs := float64(time.Since(queryStart).Nanoseconds()) / 1e6

		stats.Record(ctx, MeasureLatencyMs.M(timeSpentMs))
	}

	stats.Record(ctx, MeasureQueryCount.M(1))
}

func (c *callbacks) beforeCreateV2(db *gormv2.DB)   { c.beforeV2(db, "create") }
func (c *callbacks) afterCreateV2(db *gormv2.DB)    { c.afterV2(db) }
func (c *callbacks) beforeQueryV2(db *gormv2.DB)    { c.beforeV2(db, "query") }
func (c *callbacks) afterQueryV2(db *gormv2.DB)     { c.afterV2(db) }
func (c *callbacks) beforeRowQueryV2(db *gormv2.DB) { c.beforeV2(db, "row_query") }
func (c *callbacks) afterRowQueryV2(db *gormv2.DB)  { c.afterV2(db) }
func (c *callbacks) beforeUpdateV2(db *gormv2.DB)   { c.beforeV2(db, "update") }
func (c *callbacks) afterUpdateV2(db *gormv2.DB)    { c.afterV2(db) }
func (c *callbacks) beforeDeleteV2(db *gormv2.DB)   { c.beforeV2(db, "delete") }
func (c *callbacks) afterDeleteV2(db *gormv2.DB)    { c.afterV2(db) }
