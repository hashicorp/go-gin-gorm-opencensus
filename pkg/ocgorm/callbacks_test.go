package ocgorm

// These tests tell the same story as pkg/ocgormv2/callbacks_test.go, for the
// legacy gorm v1 instrumentation: a database query must reach an observability
// backend as an outbound call that describes the database it queried.
//
// The beat that matters most is the span kind. A span with no kind arrives at an
// OpenTelemetry backend as INTERNAL, and Instana classifies internal spans as
// ordinary work without ever inspecting their db.* attributes -- so the kind is
// what makes every other attribute meaningful.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"testing"

	"github.com/jinzhu/gorm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opencensus.io/trace"
)

// stubDriver is the smallest database/sql driver that lets gorm v1 open a
// connection. The tests never execute SQL -- they drive the instrumentation
// hooks directly -- so nothing here needs to behave like a real database.
type stubDriver struct{}

func (stubDriver) Open(string) (driver.Conn, error) { return stubConn{}, nil }

type stubConn struct{}

func (stubConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (stubConn) Close() error                        { return nil }
func (stubConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

func init() { sql.Register("ocgorm-stub", stubDriver{}) }

type user struct {
	ID uint
}

// recorder collects finished spans, standing in for the exporter that would ship
// them to a tracing backend.
type recorder struct {
	spans []*trace.SpanData
}

func (r *recorder) ExportSpan(s *trace.SpanData) { r.spans = append(r.spans, s) }

// scenario builds the world a gorm v1 query runs in.
type scenario struct {
	t     *testing.T
	rec   *recorder
	scope *gorm.Scope
	c     *callbacks
}

// givenAnInstrumentedQuery sets up a query against the users table inside a
// request span.
//
// A parent span in context is not incidental -- it selects the branch of
// startTrace that production always takes, and that branch is where the span kind
// was missing.
func givenAnInstrumentedQuery(t *testing.T, opts ...Option) *scenario {
	t.Helper()

	sqlDB, err := sql.Open("ocgorm-stub", "")
	require.NoError(t, err, "opening the stub database")
	t.Cleanup(func() { _ = sqlDB.Close() })

	gormDB, err := gorm.Open("postgres", sqlDB)
	require.NoError(t, err, "opening the gorm v1 database")
	t.Cleanup(func() { _ = gormDB.Close() })

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

	scope := gormDB.NewScope(&user{})
	scope.Set(contextScopeKey, ctx)

	return &scenario{t: t, rec: rec, scope: scope, c: c}
}

// whenTheQueryRuns drives the before and after hooks around the point where gorm
// itself builds the SQL, so the test sees the same ordering production does.
func (s *scenario) whenTheQueryRuns(operation, sqlBuiltByGorm string) *scenario {
	s.t.Helper()

	s.c.before(s.scope, operation)
	// gorm builds scope.SQL inside its own "gorm:<operation>" callback, which runs
	// between the two hooks.
	s.scope.SQL = sqlBuiltByGorm
	s.c.after(s.scope)

	return s
}

// thenTheDatabaseSpan returns the span the query should have produced.
func (s *scenario) thenTheDatabaseSpan() *trace.SpanData {
	s.t.Helper()

	var spans []*trace.SpanData
	for _, span := range s.rec.spans {
		if span.Name != "parent" {
			spans = append(spans, span)
		}
	}
	require.NotEmpty(s.t, spans, "the instrumentation produced no span for the query")

	return spans[0]
}

// TestADatabaseQueryIsReportedAsAnOutboundCall:
//
//	Given a gorm v1 query running inside a request span,
//	When  it completes,
//	Then  its span is marked as a client (outbound) call.
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
//	Given a gorm v1 query with statement recording enabled,
//	When  it completes,
//	Then  its span carries the OpenTelemetry database attributes,
//	And   it still carries the Datadog attributes services depend on today.
func TestADatabaseQueryDescribesTheDatabaseItQueried(t *testing.T) {
	const sqlText = "SELECT * FROM users WHERE id = $1"

	span := givenAnInstrumentedQuery(t, Query(true)).
		whenTheQueryRuns("query", sqlText).
		thenTheDatabaseSpan()

	t.Run("OpenTelemetry attributes", func(t *testing.T) {
		assert.Equal(t, sqlText, span.Attributes[DBStatementAttribute])
		assert.Equal(t, "SELECT", span.Attributes[DBOperationAttribute],
			"db.operation should be the SQL verb, which is what backends show as the command type")
		assert.Equal(t, "users", span.Attributes[DBSQLTableAttribute])
	})

	t.Run("Datadog attributes still present", func(t *testing.T) {
		assert.Equal(t, sqlText, span.Attributes[ResourceNameAttribute],
			"services still fan out to Datadog during the migration")
		assert.Equal(t, "users", span.Attributes[TableAttribute])
	})
}

// TestOptingOutOfStatementRecordingIsHonoured:
//
//	Given a caller that disabled statement recording with Query(false),
//	When  a query completes,
//	Then  the SQL appears under no attribute at all.
func TestOptingOutOfStatementRecordingIsHonoured(t *testing.T) {
	span := givenAnInstrumentedQuery(t, Query(false)).
		whenTheQueryRuns("query", "SELECT * FROM users WHERE id = $1").
		thenTheDatabaseSpan()

	assert.NotContains(t, span.Attributes, DBStatementAttribute)
	assert.NotContains(t, span.Attributes, ResourceNameAttribute)
}

// TestSQLVerbForOperation documents the mapping that becomes db.operation,
// including the unmapped case where the attribute is omitted rather than guessed.
func TestSQLVerbForOperation(t *testing.T) {
	for operation, wantVerb := range map[string]string{
		"create":    "INSERT",
		"query":     "SELECT",
		"row_query": "SELECT",
		"update":    "UPDATE",
		"delete":    "DELETE",
		"nonsense":  "",
	} {
		t.Run(operation, func(t *testing.T) {
			assert.Equal(t, wantVerb, SQLVerbForOperation(operation))
		})
	}
}
