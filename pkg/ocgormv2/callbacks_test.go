package ocgormv2

// These tests tell the story of a gorm v2 query as an observability backend sees
// it, and they exist because of a specific outage: after services moved their
// trace export from Datadog to Instana over the OpenCensus->OpenTelemetry bridge,
// database queries silently stopped being recognised as database calls.
//
// The story has three beats, and each has a test below:
//
//  1. A query is an OUTBOUND call, so its span must say so. A span with no kind
//     arrives as INTERNAL, and Instana classifies internal spans as ordinary work
//     without ever inspecting their db.* attributes -- so the kind is what makes
//     everything else matter.
//  2. The span must DESCRIBE THE DATABASE, using the attribute names the backend
//     actually reads, while keeping the Datadog ones that are still in use.
//  3. Recording the statement must honour the caller's Query(bool) choice, and
//     must work whether gorm built the SQL or the caller supplied it.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opencensus.io/trace"
	"gorm.io/gorm"

	"github.com/hashicorp/go-gin-gorm-opencensus/pkg/ocgorm"
)

// recorder collects finished spans, standing in for the exporter that would ship
// them to a tracing backend.
type recorder struct {
	spans []*trace.SpanData
}

func (r *recorder) ExportSpan(s *trace.SpanData) { r.spans = append(r.spans, s) }

// gormSpans returns the spans the instrumentation produced, excluding the
// surrounding request span.
func (r *recorder) gormSpans() []*trace.SpanData {
	var out []*trace.SpanData
	for _, s := range r.spans {
		if s.Name != "parent" {
			out = append(out, s)
		}
	}
	return out
}

// scenario builds the world a gorm query runs in: a recording exporter, a parent
// span in context, and an instrumented statement.
type scenario struct {
	t   *testing.T
	rec *recorder
	db  *gorm.DB
	c   *callbacks
}

// givenAnInstrumentedQuery sets up a query against the "users" table inside a
// request span.
//
// The gorm.DB is built directly rather than opened over a driver: these callbacks
// only read Statement.Table, Statement.SQL and Statement.Context, so a real
// connection would add a dialector dependency without covering anything more.
//
// A parent span in context is not incidental -- it selects the branch of
// startTrace that production always takes, and that branch is where the span kind
// was missing.
func givenAnInstrumentedQuery(t *testing.T, opts ...Option) *scenario {
	t.Helper()

	rec := &recorder{}
	trace.RegisterExporter(rec)
	t.Cleanup(func() { trace.UnregisterExporter(rec) })

	ctx, parent := trace.StartSpan(context.Background(), "parent",
		trace.WithSampler(trace.AlwaysSample()))
	t.Cleanup(parent.End)

	c := &callbacks{defaultAttributes: []trace.Attribute{}}
	for _, o := range opts {
		o.apply(c)
	}

	return &scenario{
		t:   t,
		rec: rec,
		db:  &gorm.DB{Statement: &gorm.Statement{Table: "users", Context: ctx}},
		c:   c,
	}
}

// andTheCallerSuppliedTheSQL puts the statement in place before the query runs,
// the way db.Raw(...).Find(...) does.
func (s *scenario) andTheCallerSuppliedTheSQL(sql string) *scenario {
	s.db.Statement.SQL.WriteString(sql)
	return s
}

// whenTheQueryRuns drives the before and after hooks around the point where gorm
// itself builds the SQL, so the test sees the same ordering production does.
func (s *scenario) whenTheQueryRuns(operation, sqlBuiltByGorm string) *scenario {
	s.t.Helper()

	s.c.before(s.db, operation)
	if sqlBuiltByGorm != "" {
		// gorm builds Statement.SQL inside its own "gorm:<operation>" callback,
		// which runs between the two hooks.
		s.db.Statement.SQL.Reset()
		s.db.Statement.SQL.WriteString(sqlBuiltByGorm)
	}
	s.c.after(s.db)

	return s
}

// thenTheDatabaseSpan returns the single span the query should have produced,
// failing the test if none arrived.
//
// Selected by presence rather than by count: ocgormv2 registers its row-query
// callbacks against an anchor that does not exist on gorm v2's Query processor,
// so a real query currently records more than one span. That is a separate
// pre-existing defect, and asserting a count here would fail because of it.
func (s *scenario) thenTheDatabaseSpan() *trace.SpanData {
	s.t.Helper()

	spans := s.rec.gormSpans()
	require.NotEmpty(s.t, spans, "the instrumentation produced no span for the query")

	return spans[0]
}

// TestADatabaseQueryIsReportedAsAnOutboundCall is the regression test for the
// outage:
//
//	Given a gorm query running inside a request span,
//	When  it completes,
//	Then  its span is marked as a client (outbound) call.
//
// Before this was fixed the kind was only set when no parent span existed, which
// never happens in a served request -- and never happens at all once the
// OpenCensus->OpenTelemetry bridge is installed, because the bridge's FromContext
// returns a no-op span rather than nil.
func TestADatabaseQueryIsReportedAsAnOutboundCall(t *testing.T) {
	span := givenAnInstrumentedQuery(t, Query(true)).
		whenTheQueryRuns("query", "SELECT * FROM users WHERE id = $1").
		thenTheDatabaseSpan()

	assert.Equal(t, trace.SpanKindClient, span.SpanKind,
		"a query without client kind reaches the backend as INTERNAL, and Instana "+
			"then reports it as ordinary internal work rather than a database call")
}

// TestADatabaseQueryDescribesTheDatabaseItQueried:
//
//	Given a gorm query with statement recording enabled,
//	When  it completes,
//	Then  its span carries the OpenTelemetry database attributes,
//	And   it still carries the Datadog attributes services depend on today.
//
// The db.* keys are the pre-1.26 semantic-convention spellings deliberately; see
// the constants in pkg/ocgorm/trace.go for why renaming them breaks detection.
func TestADatabaseQueryDescribesTheDatabaseItQueried(t *testing.T) {
	const sql = "SELECT * FROM users WHERE id = $1"

	span := givenAnInstrumentedQuery(t, Query(true)).
		whenTheQueryRuns("query", sql).
		thenTheDatabaseSpan()

	t.Run("OpenTelemetry attributes", func(t *testing.T) {
		assert.Equal(t, sql, span.Attributes[ocgorm.DBStatementAttribute])
		assert.Equal(t, "SELECT", span.Attributes[ocgorm.DBOperationAttribute],
			"db.operation should be the SQL verb, which is what backends show as the command type")
		assert.Equal(t, "users", span.Attributes[ocgorm.DBSQLTableAttribute])
	})

	t.Run("Datadog attributes still present", func(t *testing.T) {
		assert.Equal(t, sql, span.Attributes[ocgorm.ResourceNameAttribute],
			"services still fan out to Datadog during the migration")
		assert.Equal(t, "users", span.Attributes[ocgorm.TableAttribute])
	})
}

// TestAStatementSuppliedByTheCallerIsStillRecorded covers the query shape that
// makes the before-hook recording necessary:
//
//	Given a query whose SQL the caller built, as db.Raw(...).Find(...) does,
//	When  it completes,
//	Then  the statement is still recorded.
//
// gorm normally builds Statement.SQL after the before-hook, so for Find, Create,
// Update and Delete only the after-hook sees it. Raw populates it up front. The
// statement is recorded in both hooks so either shape is covered.
func TestAStatementSuppliedByTheCallerIsStillRecorded(t *testing.T) {
	const sql = "SELECT * FROM users WHERE id = $1"

	span := givenAnInstrumentedQuery(t, Query(true)).
		andTheCallerSuppliedTheSQL(sql).
		whenTheQueryRuns("query", "").
		thenTheDatabaseSpan()

	assert.Equal(t, sql, span.Attributes[ocgorm.DBStatementAttribute])
	assert.Equal(t, sql, span.Attributes[ocgorm.ResourceNameAttribute])
}

// TestOptingOutOfStatementRecordingIsHonoured:
//
//	Given a caller that disabled statement recording with Query(false),
//	When  a query completes,
//	Then  the SQL appears under no attribute at all.
//
// Worth pinning explicitly, because adding db.statement alongside resource.name
// would otherwise be an easy way to leak the SQL of callers who opted out.
func TestOptingOutOfStatementRecordingIsHonoured(t *testing.T) {
	span := givenAnInstrumentedQuery(t, Query(false)).
		whenTheQueryRuns("query", "SELECT * FROM users WHERE id = $1").
		thenTheDatabaseSpan()

	assert.NotContains(t, span.Attributes, ocgorm.DBStatementAttribute)
	assert.NotContains(t, span.Attributes, ocgorm.ResourceNameAttribute)
}

// TestEveryGormOperationReportsItsSQLVerb walks the operations gorm can hand the
// instrumentation:
//
//	Given each of gorm's callback operations,
//	When  a query of that kind completes,
//	Then  db.operation is the corresponding SQL verb.
func TestEveryGormOperationReportsItsSQLVerb(t *testing.T) {
	for operation, wantVerb := range map[string]string{
		"create":    "INSERT",
		"query":     "SELECT",
		"row_query": "SELECT",
		"update":    "UPDATE",
		"delete":    "DELETE",
	} {
		t.Run(operation, func(t *testing.T) {
			span := givenAnInstrumentedQuery(t, Query(true)).
				whenTheQueryRuns(operation, "SELECT 1").
				thenTheDatabaseSpan()

			assert.Equal(t, wantVerb, span.Attributes[ocgorm.DBOperationAttribute])
		})
	}
}
