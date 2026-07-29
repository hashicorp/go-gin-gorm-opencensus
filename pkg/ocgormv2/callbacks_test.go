package ocgormv2

import (
	"context"
	"testing"

	"go.opencensus.io/trace"
	"gorm.io/gorm"

	"github.com/hashicorp/go-gin-gorm-opencensus/pkg/ocgorm"
)

// recorder collects finished spans so a test can inspect them the way an
// exporter would.
type recorder struct {
	spans []*trace.SpanData
}

func (r *recorder) ExportSpan(s *trace.SpanData) { r.spans = append(r.spans, s) }

// record runs one instrumented gorm operation against a hand-built statement and
// returns the spans that were exported.
//
// The gorm.DB is constructed directly rather than opened over a driver: these
// callbacks only read Statement.Table/SQL/Context, so a real connection would add
// a dialector dependency without covering anything extra.
func record(t *testing.T, c *callbacks, operation, sql string) []*trace.SpanData {
	t.Helper()

	rec := &recorder{}
	trace.RegisterExporter(rec)
	t.Cleanup(func() { trace.UnregisterExporter(rec) })

	// A parent span in context is what production has, and it selects the
	// branch of startTrace that the bug was in.
	ctx, parent := trace.StartSpan(context.Background(), "parent",
		trace.WithSampler(trace.AlwaysSample()))

	db := &gorm.DB{Statement: &gorm.Statement{Table: "users", Context: ctx}}

	c.before(db, operation)
	// gorm builds Statement.SQL in its own "gorm:<operation>" callback, which
	// runs between the before and after hooks. Standing in for that here is what
	// makes the empty-statement-in-startTrace trap visible.
	db.Statement.SQL.WriteString(sql)
	c.after(db)

	parent.End()

	var gormSpans []*trace.SpanData
	for _, s := range rec.spans {
		if s.Name != "parent" {
			gormSpans = append(gormSpans, s)
		}
	}
	return gormSpans
}

func attrs(s *trace.SpanData) map[string]interface{} { return s.Attributes }

// TestQuerySpanIsClientKindForInstana is the regression test for the defect that
// hid every gorm query from Instana's database views:
//
//	Given a parent span in context, as every instrumented request has,
//	When  an instrumented gorm query runs,
//	Then  the span it produces is SpanKindClient.
//
// A database query is an outbound call. Before this was fixed the kind was only
// set on the no-parent branch, so real traffic produced kind-unspecified spans;
// those reach an OpenTelemetry backend as INTERNAL, and Instana then treats the
// span as internal work and never inspects its db.* attributes.
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

// TestQuerySpanCarriesOtelDatabaseAttributes pins the attribute contract:
//
//	Given an instrumented gorm query with statement recording enabled,
//	When  it completes,
//	Then  the span carries the OpenTelemetry database attributes, and still
//	      carries the pre-existing Datadog ones.
//
// The db.* keys are the pre-1.26 semantic-convention spellings on purpose; see
// the constants in pkg/ocgorm/trace.go.
func TestQuerySpanCarriesOtelDatabaseAttributes(t *testing.T) {
	const sql = "SELECT * FROM users WHERE id = $1"
	spans := record(t, &callbacks{query: true}, "query", sql)

	if len(spans) == 0 {
		t.Fatal("no span was exported for the gorm operation")
	}
	got := attrs(spans[0])

	for key, want := range map[string]interface{}{
		ocgorm.DBStatementAttribute: sql,
		ocgorm.DBOperationAttribute: "SELECT",
		ocgorm.DBSQLTableAttribute:  "users",
		// Datadog attributes must survive: services still fan out to Datadog
		// during the Instana migration.
		ocgorm.ResourceNameAttribute: sql,
		ocgorm.TableAttribute:        "users",
	} {
		if got[key] != want {
			t.Errorf("attribute %s = %v, want %v", key, got[key], want)
		}
	}
}

// TestStatementOmittedWhenQueryDisabled checks the Query(false) contract still
// holds for the new attribute: callers who opt out of statement recording must
// not have the SQL emitted under a different key.
func TestStatementOmittedWhenQueryDisabled(t *testing.T) {
	spans := record(t, &callbacks{query: false}, "query", "SELECT * FROM users WHERE id = $1")

	if len(spans) == 0 {
		t.Fatal("no span was exported for the gorm operation")
	}
	for _, s := range spans {
		if _, ok := attrs(s)[ocgorm.DBStatementAttribute]; ok {
			t.Errorf("span %q recorded %s despite Query(false)", s.Name, ocgorm.DBStatementAttribute)
		}
		if _, ok := attrs(s)[ocgorm.ResourceNameAttribute]; ok {
			t.Errorf("span %q recorded %s despite Query(false)", s.Name, ocgorm.ResourceNameAttribute)
		}
	}
}

// TestOperationVerbs covers the gorm-operation to SQL-verb mapping that becomes
// db.operation, including the unmapped case.
func TestOperationVerbs(t *testing.T) {
	for operation, want := range map[string]string{
		"create":    "INSERT",
		"query":     "SELECT",
		"row_query": "SELECT",
		"update":    "UPDATE",
		"delete":    "DELETE",
		"nonsense":  "",
	} {
		if got := ocgorm.SQLVerbForOperation(operation); got != want {
			t.Errorf("SQLVerbForOperation(%q) = %q, want %q", operation, got, want)
		}
	}
}
