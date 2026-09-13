package prices

import (
	"context"
	"testing"
)

// mustCount is the number of rows in unit_price.
//
// In export_test.go rather than in store.go: a file with the _test suffix is compiled
// only for this package's tests, so it reaches the unexported db handle without shipping
// a production method that takes a testing-shaped interface. It used to be the latter,
// which put a *testing.T-shaped parameter in the exported surface of a package that
// writes money.
func (s *SQLiteStore) mustCount(t *testing.T, ctx context.Context) int {
	t.Helper()
	var n int
	if err := s.db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM unit_price`).Scan(&n); err != nil {
		t.Fatalf("count unit_price: %v", err)
	}
	return n
}
