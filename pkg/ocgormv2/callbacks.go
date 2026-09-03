package ocgormv2

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"go.opencensus.io/stats"
	"go.opencensus.io/tag"
	"go.opencensus.io/trace"
	"gorm.io/gorm"

	"github.com/hashicorp/go-gin-gorm-opencensus/pkg/ocgorm"
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
		// Row queries live on gorm v2's Row processor, whose built-in callback is
		// named "gorm:row" -- the Query processor has only gorm:query,
		// gorm:preload and gorm:after_query. Anchoring these to "gorm:row_query"
		// on Query matched nothing, and gorm appends callbacks whose anchor it
		// cannot find rather than rejecting them, so they silently ran on every
		// Query (recording a second, duplicate span) and never ran for the
		// Row()/Rows()/Scan() calls they were meant to instrument.
		db.Callback().Row().Before("gorm:row").Register("instrumentation:before_row_query", c.beforeRowQuery),
		db.Callback().Row().After("gorm:row").Register("instrumentation:after_row_query", c.afterRowQuery),
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

	// SpanKindClient must be set on BOTH branches. A database query is an
	// outbound call, and a span without an explicit kind reaches an
	// OpenTelemetry backend as INTERNAL -- Instana then classifies it as an
	// internal span and never looks at the db.* attributes below, so the query
	// disappears from its database views.
	//
	// The parentSpan == nil branch alone is not enough: once the
	// OpenCensus->OpenTelemetry bridge is installed it owns trace.DefaultTracer,
	// and its FromContext never returns nil (it wraps a no-op span), so on the
	// bridged path this code always takes the branch below.
	if parentSpan == nil {
		ctx, span = trace.StartSpan(
			context.Background(),
			fmt.Sprintf("gorm:%s", operation),
			trace.WithSpanKind(trace.SpanKindClient),
			trace.WithSampler(c.startOptions.Sampler),
		)
	} else {
		ctx, span = trace.StartSpan(
			ctx,
			fmt.Sprintf("gorm:%s", operation),
			trace.WithSpanKind(trace.SpanKindClient),
		)
	}

	attributes := slices.Clone(c.defaultAttributes)
	attributes = append(attributes,
		trace.StringAttribute(ocgorm.TableAttribute, db.Statement.Table),
		trace.StringAttribute(ocgorm.DBSQLTableAttribute, db.Statement.Table),
	)

	if verb := ocgorm.SQLVerbForOperation(operation); verb != "" {
		attributes = append(attributes, trace.StringAttribute(ocgorm.DBOperationAttribute, verb))
	}

	// db.statement tracks resource.name exactly, in both hooks, so the two never
	// disagree about what the statement was.
	//
	// Whether the statement exists yet depends on how the caller built the query.
	// gorm normally populates Statement.SQL inside the "gorm:<operation>" callback,
	// which runs after this hook, so for Find/Create/Update/Delete it is still
	// empty here and only the endTrace copy carries a value. When the caller
	// supplied the SQL itself -- db.Raw(...).Find(...) -- it is already populated.
	// Recording in both hooks covers both shapes.
	if c.query {
		statement := db.Statement.SQL.String()
		attributes = append(attributes,
			trace.StringAttribute(ocgorm.ResourceNameAttribute, statement),
			trace.StringAttribute(ocgorm.DBStatementAttribute, statement),
		)
	}

	span.AddAttributes(attributes...)

	return ctx
}

func (c *callbacks) endTrace(db *gorm.DB) {
	span := trace.FromContext(db.Statement.Context)

	// Add query to the span if requested. This runs after the gorm callback that
	// builds Statement.SQL, so the statement is populated here for every operation
	// -- which is why the startTrace copy alone is not sufficient.
	//
	// gorm keeps bound values in Statement.Vars and leaves placeholders in
	// Statement.SQL, so this records parameterised SQL rather than literals.
	// Keep it that way: backends are not guaranteed to scrub or truncate this
	// value, so rendering the values in would leak them verbatim.
	if c.query {
		statement := db.Statement.SQL.String()
		span.AddAttributes(
			trace.StringAttribute(ocgorm.ResourceNameAttribute, statement),
			trace.StringAttribute(ocgorm.DBStatementAttribute, statement),
		)
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
		tag.Upsert(ocgorm.Operation, operation),
		tag.Upsert(ocgorm.Table, db.Statement.Table),
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

		stats.Record(ctx, ocgorm.MeasureLatencyMs.M(timeSpentMs))
	}

	stats.Record(ctx, ocgorm.MeasureQueryCount.M(1))
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
