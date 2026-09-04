package storage

import (
	"context"
	"strings"
	"testing"
)

// The index exists to make paging a seek rather than a scan, and the two
// candidate spellings of the cursor differ on exactly that — the expanded
// `a > ? OR (a = ? AND b > ?)` form plans as a full SCAN. Nothing else in
// the suite would notice the difference, so it is asserted directly.
func TestCursorPlanSeeksRatherThanScans(t *testing.T) {
	db := openTestDB(t)
	seedPagingBooks(t, db, "alpha", "bravo", "charlie")

	query, args := BookPage{AfterTitle: "alpha", AfterID: 1, Limit: 5}.paginate(
		`SELECT id FROM books`, "WHERE", "sort_title", "id")

	rows, err := db.Read().QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var plan []string
	for rows.Next() {
		var a, b, c int
		var detail string
		if err := rows.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	joined := strings.Join(plan, " | ")

	if !strings.Contains(joined, "SEARCH") || !strings.Contains(joined, "books_sort_title_id") {
		t.Errorf("cursor plan = %q, want a SEARCH against books_sort_title_id — the index migration 2026090309 exists for", joined)
	}
	if strings.Contains(joined, "SCAN books") {
		t.Errorf("cursor plan = %q, want a seek, not a scan of every earlier row", joined)
	}
}
