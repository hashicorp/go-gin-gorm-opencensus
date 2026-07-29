package ocgormv2

// These tests tell the story of WHERE the instrumentation hooks itself into gorm.
//
// gorm v2 routes work through separate callback processors -- Create, Query, Row,
// Update, Delete -- and each has its own built-in callback to anchor against. Get
// the processor or the anchor name wrong and gorm does not complain: an anchor it
// cannot resolve is simply appended to whichever processor you named. The hooks
// then run, but for the wrong operations.
//
// That is what happened with row queries. They were registered on the Query
// processor against "gorm:row_query", a name that only exists on gorm v1. The
// result was a span recorded twice for every ordinary query and no span at all for
// the Row()/Rows()/Scan() calls the hooks were meant to cover.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// givenInstrumentationRegistered returns a gorm DB with the callbacks installed.
//
// No dialector is needed: registration only touches gorm's callback registry, and
// gorm.Open skips driver initialisation when the dialector is nil. That keeps this
// test free of any database driver dependency.
func givenInstrumentationRegistered(t *testing.T) *gorm.DB {
	t.Helper()

	db, err := gorm.Open(nil, &gorm.Config{})
	require.NoError(t, err, "opening a driverless gorm DB")
	require.NoError(t, RegisterCallbacks(db), "registering instrumentation callbacks")

	return db
}

// TestRowQueriesAreInstrumentedOnTheRowProcessor:
//
//	Given the instrumentation registered on a gorm v2 DB,
//	When  the Row processor's callbacks are inspected,
//	Then  the row-query hooks are there.
//
// Row(), Rows() and Scan() all execute through this processor, so hooks registered
// anywhere else never see them.
func TestRowQueriesAreInstrumentedOnTheRowProcessor(t *testing.T) {
	db := givenInstrumentationRegistered(t)

	assert.NotNil(t, db.Callback().Row().Get("instrumentation:before_row_query"),
		"Row()/Rows()/Scan() run through the Row processor and would otherwise be untraced")
	assert.NotNil(t, db.Callback().Row().Get("instrumentation:after_row_query"))
}

// TestOrdinaryQueriesAreNotInstrumentedTwice:
//
//	Given the instrumentation registered on a gorm v2 DB,
//	When  the Query processor's callbacks are inspected,
//	Then  only the query hooks are present, not the row-query ones.
//
// This is the half of the defect that was visible in traces: the row-query hooks
// sat on the Query processor with an unresolvable anchor, so gorm appended them and
// every Find recorded a second, duplicate span alongside gorm:query.
func TestOrdinaryQueriesAreNotInstrumentedTwice(t *testing.T) {
	db := givenInstrumentationRegistered(t)

	require.NotNil(t, db.Callback().Query().Get("instrumentation:before_query"),
		"the query hooks themselves must stay on the Query processor")

	assert.Nil(t, db.Callback().Query().Get("instrumentation:before_row_query"),
		"row-query hooks on the Query processor make every query record a duplicate span")
	assert.Nil(t, db.Callback().Query().Get("instrumentation:after_row_query"))
}

// TestEveryOperationIsInstrumentedOnItsOwnProcessor guards the remaining
// registrations against the same class of mistake:
//
//	Given the instrumentation registered on a gorm v2 DB,
//	When  each processor is inspected,
//	Then  it carries exactly the hooks for its own operation.
func TestEveryOperationIsInstrumentedOnItsOwnProcessor(t *testing.T) {
	db := givenInstrumentationRegistered(t)

	// gorm's processor type is unexported, so each entry is that processor's Get
	// method rather than the processor itself.
	for operation, lookup := range map[string]func(string) func(*gorm.DB){
		"create":    db.Callback().Create().Get,
		"query":     db.Callback().Query().Get,
		"row_query": db.Callback().Row().Get,
		"update":    db.Callback().Update().Get,
		"delete":    db.Callback().Delete().Get,
	} {
		t.Run(operation, func(t *testing.T) {
			assert.NotNil(t, lookup("instrumentation:before_"+operation),
				"before hook missing from the %s operation's processor", operation)
			assert.NotNil(t, lookup("instrumentation:after_"+operation),
				"after hook missing from the %s operation's processor", operation)
		})
	}
}
