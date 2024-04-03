package ocgorm

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opencensus.io/stats"
	"go.opencensus.io/tag"
	"go.opencensus.io/trace"
	"gorm.io/gorm"
)

// Option allows for managing ocgorm configuration using functional options.
type Option interface {
	apply(c *callbacks)
}

// OptionFunc converts a regular function to an Option if it's definition is compatible.
type OptionFunc func(c *callbacks)

func (fn OptionFunc) apply(c *callbacks) {
	fn(c)
}

// AllowRoot allows creating root spans in the absence of existing spans.
type AllowRoot bool

func (a AllowRoot) apply(c *callbacks) {
	c.allowRoot = bool(a)
}

// Query allows recording the sql queries in spans.
type Query bool

func (q Query) apply(c *callbacks) {
	c.query = bool(q)
}

// StartOptions configures the initial options applied to a span.
func StartOptions(o trace.StartOptions) Option {
	return OptionFunc(func(c *callbacks) {
		c.startOptions = o
	})
}

// DefaultAttributes sets attributes to each span.
type DefaultAttributes []trace.Attribute

func (d DefaultAttributes) apply(c *callbacks) {
	c.defaultAttributes = []trace.Attribute(d)
}

type callbacks struct {
	// Allow ocgorm to create root spans absence of existing spans or even context.
	// Default is to not trace ocgorm calls if no existing parent span is found
	// in context.
	allowRoot bool

	// Allow recording of sql queries in spans.
	// Only allow this if it is safe to have queries recorded with respect to
	// security.
	query bool

	// startOptions are applied to the span started around each request.
	//
	// StartOptions.SpanKind will always be set to trace.SpanKindClient.
	startOptions trace.StartOptions

	// DefaultAttributes will be set to each span as default.
	defaultAttributes []trace.Attribute
}

// RegisterCallbacks registers the necessary callbacks in Gorm's hook system for instrumentation.
func RegisterCallbacks(db *gorm.DB, opts ...Option) error {
	c := &callbacks{
		defaultAttributes: []trace.Attribute{},
	}

	for _, opt := range opts {
		opt.apply(c)
	}

	return errors.Join(
		db.Callback().Create().Before("gorm:create").Register("instrumentation:before_create", c.beforeCreate),
		db.Callback().Create().After("gorm:create").Register("instrumentation:after_create", c.afterCreate),
		db.Callback().Query().Before("gorm:query").Register("instrumentation:before_query", c.beforeQuery),
		db.Callback().Query().After("gorm:query").Register("instrumentation:after_query", c.afterQuery),
		db.Callback().Query().Before("gorm:row_query").Register("instrumentation:before_row_query", c.beforeRowQuery),
		db.Callback().Query().After("gorm:row_query").Register("instrumentation:after_row_query", c.afterRowQuery),
		db.Callback().Update().Before("gorm:update").Register("instrumentation:before_update", c.beforeUpdate),
		db.Callback().Update().After("gorm:update").Register("instrumentation:after_update", c.afterUpdate),
		db.Callback().Delete().Before("gorm:delete").Register("instrumentation:before_delete", c.beforeDelete),
		db.Callback().Delete().After("gorm:delete").Register("instrumentation:after_delete", c.afterDelete))
}

func (c *callbacks) before(db *gorm.DB, operation string) {
	ctx := db.Statement.Context
	if ctx == nil {
		ctx = context.Background()
	}

	ctx = c.startTrace(ctx, db, operation)
	ctx = c.startStats(ctx, db, operation)

	db.Statement.Context = ctx
}

func (c *callbacks) after(db *gorm.DB) {
	c.endTrace(db)
	c.endStats(db)
}

func (c *callbacks) startTrace(ctx context.Context, db *gorm.DB, operation string) context.Context {
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
			fmt.Sprintf("gorm:%s", operation),
			trace.WithSpanKind(trace.SpanKindClient),
			trace.WithSampler(c.startOptions.Sampler),
		)
	} else {
		ctx, span = trace.StartSpan(ctx, fmt.Sprintf("gorm:%s", operation))
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

func (c *callbacks) endTrace(db *gorm.DB) {
	span := trace.FromContext(db.Statement.Context)

	// Add query to the span if requested
	if c.query {
		span.AddAttributes(trace.StringAttribute(ResourceNameAttribute, db.Statement.SQL.String()))
	}

	var status trace.Status

	if db.Error != nil {
		if errors.Is(db.Error, gorm.ErrRecordNotFound) {
			status.Code = trace.StatusCodeNotFound
		} else {
			status.Code = trace.StatusCodeUnknown
		}

		status.Message = db.Error.Error()
	}

	span.SetStatus(status)

	span.End()
}

var (
	queryStartPropagator, _ = tag.NewKey("sql.query_start")
)

func (c *callbacks) startStats(ctx context.Context, db *gorm.DB, operation string) context.Context {
	ctx, _ = tag.New(ctx,
		tag.Upsert(Operation, operation),
		tag.Upsert(Table, db.Statement.Table),
		tag.Upsert(queryStartPropagator, time.Now().UTC().Format(time.RFC3339Nano)),
	)

	return ctx
}

func (c *callbacks) endStats(db *gorm.DB) {
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

func (c *callbacks) beforeCreate(db *gorm.DB)   { c.before(db, "create") }
func (c *callbacks) afterCreate(db *gorm.DB)    { c.after(db) }
func (c *callbacks) beforeQuery(db *gorm.DB)    { c.before(db, "query") }
func (c *callbacks) afterQuery(db *gorm.DB)     { c.after(db) }
func (c *callbacks) beforeRowQuery(db *gorm.DB) { c.before(db, "row_query") }
func (c *callbacks) afterRowQuery(db *gorm.DB)  { c.after(db) }
func (c *callbacks) beforeUpdate(db *gorm.DB)   { c.before(db, "update") }
func (c *callbacks) afterUpdate(db *gorm.DB)    { c.after(db) }
func (c *callbacks) beforeDelete(db *gorm.DB)   { c.before(db, "delete") }
func (c *callbacks) afterDelete(db *gorm.DB)    { c.after(db) }
