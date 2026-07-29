package ocgorm

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"testing"

	"github.com/jinzhu/gorm"
	"go.opencensus.io/trace"
)

// stubDriver is the smallest database/sql driver that lets gorm v1 open a
// connection. The tests never execute SQL -- they drive the instrumentation
// callbacks directly -- so nothing here needs to behave like a real database.
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

// recorder collects finished spans so a test can inspect them the way an
// exporter would.
type recorder struct {
	spans []*trace.SpanData
}

func (r *recorder) ExportSpan(s *trace.SpanData) { r.spans = append(r.spans, s) }

// record drives one instrumented gorm v1 operation and returns the spans that
// were exported for it.
func record(t *testing.T, c *callbacks, operation, sqlText string) []*trace.SpanData {
	t.Helper()

	sqlDB, err := sql.Open("ocgorm-stub", "")
	if err != nil {
		t.Fatalf("opening stub database: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	gormDB, err := gorm.Open("postgres", sqlDB)
	if err != nil {
		t.Fatalf("opening gorm v1 database: %v", err)
	}
	t.Cleanup(func() { _ = gormDB.Close() })

	rec := &recorder{}
	trace.RegisterExporter(rec)
	t.Cleanup(func() { trace.UnregisterExporter(rec) })

	// A parent span in context is what production has, and it selects the branch
	// of startTrace that the bug was in.
	ctx, parent := trace.StartSpan(context.Background(), "parent",
		trace.WithSampler(trace.AlwaysSample()))

	scope := gormDB.NewScope(&user{})
	scope.Set(contextScopeKey, ctx)

	c.before(scope, operation)
	// gorm builds scope.SQL in its own "gorm:<operation>" callback, which runs
	// between the before and after hooks.
	scope.SQL = sqlText
	c.after(scope)

	parent.End()

	var gormSpans []*trace.SpanData
	for _, s := range rec.spans {
		if s.Name != "parent" {
			gormSpans = append(gormSpans, s)
		}
	}
	return gormSpans
}

// TestQuerySpanIsClientKindForInstana is the v1 counterpart of the ocgormv2
// regression test:
//
//	Given a parent span in context, as every instrumented request has,
//	When  an instrumented gorm query runs,
//	Then  the span it produces is SpanKindClient.
//
// Without the kind, the span reaches an OpenTelemetry backend as INTERNAL and
// Instana never inspects its db.* attributes, so the query vanishes from the
// database views.
func TestQuerySpanIsClientKindForInstana(t *testing.T) {
	spans := record(t, &callbacks{query: true}, "query", "SELECT * FROM users WHERE id = $1")

	if len(spans) == 0 {
		t.Fatal("no span was exported for the gorm operation")
	}
	for _, s := range spans {
		if s.SpanKind != trace.SpanKindClient {
			t.Errorf("span %q has kind %d, want SpanKindClient (%d) -- Instana will not classify it as a database call",
				s.Name, s.SpanKind, trace.SpanKindClient)
		}
	}
}

// TestQuerySpanCarriesOtelDatabaseAttributes pins the attribute contract for v1,
// including that the pre-existing Datadog attributes survive.
func TestQuerySpanCarriesOtelDatabaseAttributes(t *testing.T) {
	const sqlText = "SELECT * FROM users WHERE id = $1"
	spans := record(t, &callbacks{query: true}, "query", sqlText)

	if len(spans) == 0 {
		t.Fatal("no span was exported for the gorm operation")
	}
	got := spans[0].Attributes

	for key, want := range map[string]interface{}{
		DBStatementAttribute:  sqlText,
		DBOperationAttribute:  "SELECT",
		DBSQLTableAttribute:   "users",
		ResourceNameAttribute: sqlText,
		TableAttribute:        "users",
	} {
		if got[key] != want {
			t.Errorf("attribute %s = %v, want %v", key, got[key], want)
		}
	}
}
