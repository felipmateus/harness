package ledger

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/felipmateus/harness/internal/testutil/testhome"
)

// dbPath returns a ledger path inside a fresh private temporary home.
func dbPath(t testing.TB) string {
	t.Helper()
	return filepath.Join(testhome.New(t).Dir, "harness.db")
}

// openAt opens a ledger at path and closes it when the test ends. Close is idempotent, so
// a test may also close it earlier.
func openAt(t testing.TB, path string, opts Options) *Ledger {
	t.Helper()
	opts.Path = path
	l, err := Open(context.Background(), opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { l.Close(context.Background()) })
	return l
}

// openTemp opens a ledger in a fresh temporary home.
func openTemp(t testing.TB, opts Options) *Ledger {
	t.Helper()
	return openAt(t, dbPath(t), opts)
}

// mustAppend appends and fails the test on error.
func mustAppend(t testing.TB, l *Ledger, e NewEvent) Event {
	t.Helper()
	ev, _, err := l.Append(context.Background(), e)
	if err != nil {
		t.Fatalf("Append(%s/%s): %v", e.Stream, e.Type, err)
	}
	return ev
}

// appendTicks appends n small events to stream.
func appendTicks(t testing.TB, l *Ledger, stream string, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		mustAppend(t, l, NewEvent{Stream: stream, Type: "tick", Data: []byte(fmt.Sprintf(`{"n":%d}`, i))})
	}
}

// jsonString returns a valid JSON string value of exactly size bytes (quotes included).
func jsonString(size int) []byte {
	return []byte(`"` + strings.Repeat("a", size-2) + `"`)
}

// countRows counts rows of events through the writer pool.
func countRows(t testing.TB, l *Ledger, where string, args ...any) int {
	t.Helper()
	var n int
	q := `SELECT COUNT(*) FROM events`
	if where != "" {
		q += ` WHERE ` + where
	}
	if err := l.rdb.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}
