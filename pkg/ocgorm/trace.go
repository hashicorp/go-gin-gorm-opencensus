package ocgorm

// Attributes recorded on the span for the queries.
const (
	// Datadog expects the query text here to enable aggregations of queries
	// Must be used in tandem with a service.name and span.type attribute
	// Our fork uses this instead of gorm.query
	ResourceNameAttribute = "resource.name"

	TableAttribute = "gorm.table"
)

// OpenTelemetry database semantic-convention attributes.
//
// These are the pre-1.26 spellings, and that is deliberate. Instana's OTLP
// ingestion reads exactly these keys and has no fallback to the newer
// db.system.name / db.query.text / db.namespace / db.operation.name names --
// emitting those instead means the span is never recognised as a database call
// at all. Do not "modernise" these constants without first confirming the
// backend reads the new names.
//
// DBSystemAttribute is the key that triggers database detection; without it the
// remaining attributes are ignored. It is not set by these callbacks -- the
// dialect is known by the caller, so it is supplied via DefaultAttributes.
const (
	DBSystemAttribute    = "db.system"
	DBStatementAttribute = "db.statement"
	DBOperationAttribute = "db.operation"

	// DBSQLTableAttribute is emitted for OpenTelemetry correctness and for
	// backends that read it. Instana itself does not.
	DBSQLTableAttribute = "db.sql.table"
)

// SQLVerbForOperation maps a gorm callback operation to the SQL verb reported as
// db.operation. Instana surfaces this as the call's command type, where the
// convention is the SQL keyword ("SELECT") rather than gorm's internal name
// ("query"). Unknown operations yield "", and the attribute is then omitted.
func SQLVerbForOperation(operation string) string {
	switch operation {
	case "create":
		return "INSERT"
	case "query", "row_query":
		return "SELECT"
	case "update":
		return "UPDATE"
	case "delete":
		return "DELETE"
	default:
		return ""
	}
}
