package ocgorm

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opencensus.io/stats"
	"go.opencensus.io/tag"
	"go.opencensus.io/trace"
	gormv2 "gorm.io/gorm"
)

// RegisterCallbacksV2 registers the necessary callbacks in Gorm's hook system for instrumentation.
func RegisterCallbacksV2(dbv2 *gormv2.DB, opts ...Option) error {
	c := &callbacks{
		defaultAttributes: []trace.Attribute{},
	}

	for _, opt := range opts {
		opt.apply(c)
	}

	return errors.Join(
		dbv2.Callback().Create().Before("gorm:create").Register("instrumentation:before_create", c.beforeCreate),
		dbv2.Callback().Create().After("gorm:create").Register("instrumentation:after_create", c.afterCreate),
		dbv2.Callback().Query().Before("gorm:query").Register("instrumentation:before_query", c.beforeQuery),
		dbv2.Callback().Query().After("gorm:query").Register("instrumentation:after_query", c.afterQuery),
		dbv2.Callback().Query().Before("gorm:row_query").Register("instrumentation:before_row_query", c.beforeRowQuery),
		dbv2.Callback().Query().After("gorm:row_query").Register("instrumentation:after_row_query", c.afterRowQuery),
		dbv2.Callback().Update().Before("gorm:update").Register("instrumentation:before_update", c.beforeUpdate),
		dbv2.Callback().Update().After("gorm:update").Register("instrumentation:after_update", c.afterUpdate),
		dbv2.Callback().Delete().Before("gorm:delete").Register("instrumentation:before_delete", c.beforeDelete),
		dbv2.Callback().Delete().After("gorm:delete").Register("instrumentation:after_delete", c.afterDelete))
}

func (c *callbacks) before(db *gormv2.DB, operation string) {
	ctx := db.Statement.Context
	if ctx == nil {
		ctx = context.Background()
	}

	ctx = c.startTrace(ctx, db, operation)
	ctx = c.startStats(ctx, db, operation)

	db.Statement.Context = ctx
}

func (c *callbacks) after(db *gormv2.DB) {
	c.endTrace(db)
	c.endStats(db)
}

func (c *callbacks) startTrace(ctx context.Context, db *gormv2.DB, operation string) context.Context {
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

func (c *callbacks) endTrace(db *gormv2.DB) {
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

func (c *callbacks) startStats(ctx context.Context, db *gormv2.DB, operation string) context.Context {
	ctx, _ = tag.New(ctx,
		tag.Upsert(Operation, operation),
		tag.Upsert(Table, db.Statement.Table),
		tag.Upsert(queryStartPropagator, time.Now().UTC().Format(time.RFC3339Nano)),
	)

	return ctx
}

func (c *callbacks) endStats(db *gormv2.DB) {
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

func (c *callbacks) beforeCreate(db *gormv2.DB)   { c.before(db, "create") }
func (c *callbacks) afterCreate(db *gormv2.DB)    { c.after(db) }
func (c *callbacks) beforeQuery(db *gormv2.DB)    { c.before(db, "query") }
func (c *callbacks) afterQuery(db *gormv2.DB)     { c.after(db) }
func (c *callbacks) beforeRowQuery(db *gormv2.DB) { c.before(db, "row_query") }
func (c *callbacks) afterRowQuery(db *gormv2.DB)  { c.after(db) }
func (c *callbacks) beforeUpdate(db *gormv2.DB)   { c.before(db, "update") }
func (c *callbacks) afterUpdate(db *gormv2.DB)    { c.after(db) }
func (c *callbacks) beforeDelete(db *gormv2.DB)   { c.before(db, "delete") }
func (c *callbacks) afterDelete(db *gormv2.DB)    { c.after(db) }
