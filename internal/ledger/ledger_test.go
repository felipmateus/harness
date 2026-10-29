package ledger

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// UT-020
func TestRead_PageByLimitReportsHasMore(t *testing.T) {
	t.Parallel()
	l := openTemp(t, Options{})
	appendTicks(t, l, "s", 4)

	page, err := l.Read(context.Background(), "s", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].Seq != 2 || !page.HasMore {
		t.Fatalf("page = %+v, want [seq 2] with HasMore", page)
	}
	head, err := l.Head(context.Background(), "s")
	if err != nil || head != 4 {
		t.Fatalf("Head = %d, %v; want 4", head, err)
	}

	page, err = l.Read(context.Background(), "s", 3, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].Seq != 4 || page.HasMore {
		t.Fatalf("last page = %+v, want [seq 4] without HasMore", page)
	}
}

// UT-021
func TestRead_EmptyStreamAndAfterBeyondHead(t *testing.T) {
	t.Parallel()
	l := openTemp(t, Options{})
	ctx := context.Background()

	page, err := l.Read(ctx, "daemon", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if page.Events == nil || len(page.Events) != 0 || page.HasMore {
		t.Fatalf("empty stream page = %+v, want an empty non-nil slice", page)
	}
	if head, err := l.Head(ctx, "daemon"); err != nil || head != 0 {
		t.Fatalf("Head(empty) = %d, %v; want 0", head, err)
	}

	appendTicks(t, l, "daemon", 4)
	page, err = l.Read(ctx, "daemon", 99, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 0 || page.HasMore {
		t.Fatalf("after beyond head: %+v, want no events", page)
	}
	if head, _ := l.Head(ctx, "daemon"); head != 4 {
		t.Fatalf("Head = %d, want 4", head)
	}
}

func TestRead_RejectsNonPositiveLimit(t *testing.T) {
	t.Parallel()
	l := openTemp(t, Options{})
	if _, err := l.Read(context.Background(), "s", 0, 0); err == nil {
		t.Fatal("Read with limit 0 succeeded")
	}
}

// UT-022
func TestAppend_ConcurrentProducersGetConsecutiveSeqs(t *testing.T) {
	t.Parallel()
	const goroutines, each = 50, 20
	l := openTemp(t, Options{})

	var (
		mu   sync.Mutex
		seqs []int64
		wg   sync.WaitGroup
	)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				ev, created, err := l.Append(context.Background(), NewEvent{Stream: "t", Type: "tick"})
				if err != nil || !created {
					t.Errorf("Append: created=%v err=%v", created, err)
					return
				}
				mu.Lock()
				seqs = append(seqs, ev.Seq)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(seqs) != goroutines*each {
		t.Fatalf("got %d results, want %d", len(seqs), goroutines*each)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	for i, s := range seqs {
		if s != int64(i+1) {
			t.Fatalf("seqs[%d] = %d, want %d (gap or duplicate)", i, s, i+1)
		}
	}
	if head, _ := l.Head(context.Background(), "t"); head != goroutines*each {
		t.Fatalf("Head = %d, want %d", head, goroutines*each)
	}
}

// UT-023
func TestAppend_SameIDAndContentIsIdempotent(t *testing.T) {
	t.Parallel()
	var commits atomic.Int32
	l := openTemp(t, Options{OnCommit: func(Event) { commits.Add(1) }})
	ctx := context.Background()
	e := NewEvent{Stream: "s", ID: "X", Type: "thing.done", Data: []byte(`{"a":1}`)}

	first, created, err := l.Append(ctx, e)
	if err != nil || !created {
		t.Fatalf("first Append: created=%v err=%v", created, err)
	}
	second, created, err := l.Append(ctx, e)
	if err != nil || created {
		t.Fatalf("second Append: created=%v err=%v; want created=false", created, err)
	}
	if second.Seq != first.Seq || second.Pos != first.Pos || second.Time != first.Time || second.ID != "X" {
		t.Fatalf("second = %+v, want the stored event %+v", second, first)
	}
	if n := countRows(t, l, ""); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
	if n := commits.Load(); n != 1 {
		t.Fatalf("OnCommit called %d times, want 1 (an idempotent hit commits nothing)", n)
	}
	if head, _ := l.Head(ctx, "s"); head != 1 {
		t.Fatalf("Head = %d, want 1", head)
	}
}

// UT-024
func TestAppend_PayloadOverOneMiBIsRejected(t *testing.T) {
	t.Parallel()
	l := openTemp(t, Options{})
	ctx := context.Background()
	appendTicks(t, l, "s", 1)

	ev, created, err := l.Append(ctx, NewEvent{Stream: "s", Type: "big", Data: jsonString(1<<20 + 1)})
	if !errors.Is(err, ErrPayloadTooLarge) || created {
		t.Fatalf("Append(1 MiB + 1) = created=%v err=%v; want ErrPayloadTooLarge", created, err)
	}
	if ev.ID == "" {
		t.Fatal("rejected append returned no ID")
	}
	if head, _ := l.Head(ctx, "s"); head != 1 {
		t.Fatalf("Head = %d, want 1 (unchanged)", head)
	}

	// Exactly 1 MiB is the largest accepted payload.
	ev = mustAppend(t, l, NewEvent{Stream: "s", Type: "big", Data: jsonString(1 << 20)})
	if len(ev.Data) != 1<<20 || ev.Seq != 2 {
		t.Fatalf("1 MiB event: seq=%d len=%d", ev.Seq, len(ev.Data))
	}
}

// goose version table as written by a newer binary.
func setSchemaVersion(t *testing.T, path string, version int) {
	t.Helper()
	db, err := sql.Open(driverName, fileDSN(path, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, 1)`, version); err != nil {
		t.Fatalf("set schema version: %v", err)
	}
}

// UT-025
func TestOpen_SchemaAheadIsRefusedWithoutTouchingTheFile(t *testing.T) {
	t.Parallel()
	path := dbPath(t)
	l := openAt(t, path, Options{})
	appendTicks(t, l, "s", 2)
	if err := l.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	setSchemaVersion(t, path, 3)

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	infoBefore, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond) // a write would now move the mtime

	var checkDSN string
	got, err := Open(context.Background(), Options{Path: path, hooks: hooks{schemaCheck: func(dsn string) { checkDSN = dsn }}})
	if err == nil {
		got.Close(context.Background())
		t.Fatal("Open succeeded on a database ahead of the binary")
	}
	var ahead *SchemaAheadError
	if !errors.As(err, &ahead) || ahead.DB != 3 || ahead.Binary != 1 {
		t.Fatalf("Open error = %v; want SchemaAheadError{DB:3, Binary:1}", err)
	}
	if !errors.Is(err, ErrSchemaAhead) {
		t.Fatalf("error %v does not wrap ErrSchemaAhead", err)
	}
	if want := "ledger schema 3 is newer than this binary supports (1)"; err.Error() != want {
		t.Fatalf("message = %q, want %q", err.Error(), want)
	}

	// The check ran on a mode=ro connection: that DSN cannot write.
	if !strings.Contains(checkDSN, "mode=ro") {
		t.Fatalf("schema check DSN %q is not mode=ro", checkDSN)
	}
	probe, err := sql.Open(driverName, checkDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	if _, err := probe.Exec(`CREATE TABLE should_not_exist (x)`); err == nil || !strings.Contains(strings.ToLower(err.Error()), "readonly") {
		t.Fatalf("write on the check connection: err = %v; want a read-only refusal", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	infoAfter, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("the database file's bytes changed")
	}
	if !infoBefore.ModTime().Equal(infoAfter.ModTime()) {
		t.Fatalf("mtime changed: %v -> %v", infoBefore.ModTime(), infoAfter.ModTime())
	}
	// No migration ran: the version table still ends at 3, with its original rows.
	insp, err := Inspect(context.Background(), path)
	if err != nil || insp.SchemaVersion != 3 || !insp.SchemaAhead() {
		t.Fatalf("Inspect after refusal = %+v, %v", insp, err)
	}
}

func TestOpen_FreshAndEmptyFilesSkipTheSchemaCheck(t *testing.T) {
	t.Parallel()
	checks := 0
	hook := hooks{schemaCheck: func(string) { checks++ }}

	// Absent file.
	path := dbPath(t)
	l := openAt(t, path, Options{hooks: hook})
	l.Close(context.Background())
	if checks != 0 {
		t.Fatalf("schema check ran %d times for a new database", checks)
	}
	// Existing database: the check runs and passes.
	l = openAt(t, path, Options{hooks: hook})
	l.Close(context.Background())
	if checks != 1 {
		t.Fatalf("schema check ran %d times for an existing database, want 1", checks)
	}
	// A zero-byte file is an empty database.
	empty := dbPath(t)
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	l = openAt(t, empty, Options{hooks: hook})
	if checks != 1 {
		t.Fatalf("schema check ran for an empty file")
	}
	appendTicks(t, l, "s", 1)
}

// UT-026
func TestOnCommit_RunsOnlyAfterTheEventIsReadable(t *testing.T) {
	t.Parallel()
	var (
		l        *Ledger
		mu       sync.Mutex
		problems []string
		calls    int
	)
	l = openTemp(t, Options{OnCommit: func(ev Event) {
		head, err := l.Head(context.Background(), ev.Stream)
		mu.Lock()
		defer mu.Unlock()
		calls++
		if err != nil || head < ev.Seq {
			problems = append(problems, ev.ID)
		}
	}})
	var wg sync.WaitGroup
	for p := 0; p < 4; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				mustAppend(t, l, NewEvent{Stream: "s", Type: "tick"})
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if calls != 100 {
		t.Fatalf("OnCommit called %d times, want 100", calls)
	}
	if len(problems) > 0 {
		t.Fatalf("OnCommit ran before the commit was readable for %d events", len(problems))
	}
}

// UT-027
func TestOnCommit_SeqsPerStreamAreStrictlyConsecutive(t *testing.T) {
	t.Parallel()
	var (
		mu   sync.Mutex
		seen = map[string][]int64{}
	)
	l := openTemp(t, Options{OnCommit: func(ev Event) {
		mu.Lock()
		seen[ev.Stream] = append(seen[ev.Stream], ev.Seq)
		mu.Unlock()
	}})
	streams := []string{"a", "b"}
	var wg sync.WaitGroup
	for p := 0; p < 4; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 30; i++ {
				mustAppend(t, l, NewEvent{Stream: streams[(p+i)%2], Type: "tick"})
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	total := 0
	for stream, seqs := range seen {
		for i, s := range seqs {
			if s != int64(i+1) {
				t.Fatalf("stream %s: OnCommit call %d carried seq %d, want %d", stream, i, s, i+1)
			}
		}
		total += len(seqs)
	}
	if total != 120 {
		t.Fatalf("OnCommit saw %d events, want 120", total)
	}
}

// UT-028
func TestLast_ReturnsTheHighestSeqEventWithItsData(t *testing.T) {
	t.Parallel()
	l := openTemp(t, Options{})
	ctx := context.Background()

	if _, ok, err := l.Last(ctx, "daemon"); err != nil || ok {
		t.Fatalf("Last(empty) ok=%v err=%v; want ok=false", ok, err)
	}
	for _, pid := range []string{`{"pid":111}`, `{"pid":222}`, `{"pid":333}`} {
		mustAppend(t, l, NewEvent{Stream: "daemon", Type: "daemon.started", Data: []byte(pid)})
	}
	mustAppend(t, l, NewEvent{Stream: "other", Type: "daemon.started", Data: []byte(`{"pid":999}`)})

	ev, ok, err := l.Last(ctx, "daemon")
	if err != nil || !ok {
		t.Fatalf("Last ok=%v err=%v", ok, err)
	}
	var data struct{ PID int }
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatal(err)
	}
	if ev.Seq != 3 || ev.Type != "daemon.started" || data.PID != 333 {
		t.Fatalf("Last = seq %d type %s pid %d; want seq 3, daemon.started, pid 333", ev.Seq, ev.Type, data.PID)
	}
}

func TestStats_CountsStreamsEventsAndSchemaVersion(t *testing.T) {
	t.Parallel()
	l := openTemp(t, Options{})
	got, err := l.Stats(context.Background())
	if err != nil || (got != Stats{SchemaVersion: 1}) {
		t.Fatalf("Stats(empty) = %+v, %v", got, err)
	}
	appendTicks(t, l, "a", 3)
	appendTicks(t, l, "b", 2)
	got, err = l.Stats(context.Background())
	if err != nil || got != (Stats{SchemaVersion: 1, Streams: 2, Events: 5}) {
		t.Fatalf("Stats = %+v, %v; want 2 streams, 5 events, schema 1", got, err)
	}
	raw, _ := json.Marshal(got)
	if string(raw) != `{"schema_version":1,"streams":2,"events":5}` {
		t.Fatalf("Stats JSON = %s", raw)
	}
}

// pragmaValue reads one pragma on a specific connection.
func pragmaValue(t *testing.T, conn *sql.Conn, name string) string {
	t.Helper()
	var v string
	if err := conn.QueryRowContext(context.Background(), "PRAGMA "+name).Scan(&v); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	return strings.ToLower(v)
}

// UT-029
func TestOpen_DurabilityPragmasAreInEffectOnEveryConnection(t *testing.T) {
	t.Parallel()
	l := openTemp(t, Options{})
	ctx := context.Background()

	want := map[string]string{
		"journal_mode": "wal",
		"synchronous":  "2", // FULL
		"busy_timeout": "5000",
		"foreign_keys": "1",
	}
	if runtime.GOOS == "darwin" {
		want["fullfsync"] = "1"
		want["checkpoint_fullfsync"] = "1"
	}
	check := func(label string, conn *sql.Conn) {
		t.Helper()
		for name, expected := range want {
			if got := pragmaValue(t, conn, name); got != expected {
				t.Errorf("%s: PRAGMA %s = %s, want %s", label, name, got, expected)
			}
		}
		t.Logf("%s: fullfsync=%s checkpoint_fullfsync=%s (GOOS=%s)", label,
			pragmaValue(t, conn, "fullfsync"), pragmaValue(t, conn, "checkpoint_fullfsync"), runtime.GOOS)
	}

	wconn, err := l.wdb.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer wconn.Close()
	check("writer", wconn)
	if got := pragmaValue(t, wconn, "query_only"); got != "0" {
		t.Errorf("writer: query_only = %s, want 0", got)
	}

	// Hold all four reader connections at once so each pooled connection is inspected.
	var readers []*sql.Conn
	for i := 0; i < readerConns; i++ {
		c, err := l.rdb.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		readers = append(readers, c)
	}
	for i, c := range readers {
		check("reader "+string(rune('0'+i)), c)
		if got := pragmaValue(t, c, "query_only"); got != "1" {
			t.Errorf("reader %d: query_only = %s, want 1", i, got)
		}
		if _, err := c.ExecContext(ctx, `DELETE FROM events`); err == nil {
			t.Errorf("reader %d accepted a write", i)
		}
	}

	if n := l.wdb.Stats().MaxOpenConnections; n != 1 {
		t.Errorf("writer pool max connections = %d, want 1", n)
	}
	if n := l.rdb.Stats().MaxOpenConnections; n != 4 {
		t.Errorf("reader pool max connections = %d, want 4", n)
	}
}

// The writer's transactions are BEGIN IMMEDIATE: they hold the write lock from the start.
func TestOpen_WriterTransactionsAreImmediate(t *testing.T) {
	t.Parallel()
	l := openTemp(t, Options{})
	ctx := context.Background()

	tx, err := l.wdb.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	other, err := sql.Open(driverName, fileDSN(l.path, map[string][]string{
		"_pragma": {"busy_timeout(50)"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.ExecContext(ctx, `BEGIN IMMEDIATE`); err == nil || !strings.Contains(strings.ToLower(err.Error()), "locked") {
		t.Fatalf("second writer: err = %v; want a database-locked error while the first transaction is open", err)
	}
}

func TestOpen_CreatesTheDatabaseFilePrivate(t *testing.T) {
	t.Parallel()
	path := dbPath(t)
	l := openAt(t, path, Options{})
	appendTicks(t, l, "s", 1)

	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(path + suffix)
		if errors.Is(err, os.ErrNotExist) && suffix != "" {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s mode = %o, want 600", filepath.Base(path+suffix), perm)
		}
	}
}

func TestOpen_ExistingFileKeepsItsMode(t *testing.T) {
	t.Parallel()
	path := dbPath(t)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	openAt(t, path, Options{})
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Fatalf("existing file mode = %o, want 644 (never changed)", perm)
	}
}

func TestOpen_ReopenKeepsHistoryAndContinuesSeq(t *testing.T) {
	t.Parallel()
	path := dbPath(t)
	l := openAt(t, path, Options{})
	appendTicks(t, l, "s", 3)
	if err := l.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	l = openAt(t, path, Options{})
	ev := mustAppend(t, l, NewEvent{Stream: "s", Type: "tick"})
	if ev.Seq != 4 {
		t.Fatalf("seq after reopen = %d, want 4", ev.Seq)
	}
	page, err := l.Read(context.Background(), "s", 0, 100)
	if err != nil || len(page.Events) != 4 {
		t.Fatalf("history after reopen: %d events, %v", len(page.Events), err)
	}
}

func TestOpen_RequiresAPath(t *testing.T) {
	t.Parallel()
	if _, err := Open(context.Background(), Options{}); err == nil {
		t.Fatal("Open without a path succeeded")
	}
}

func TestMigrations_EmbeddedHeadIsVersionOne(t *testing.T) {
	t.Parallel()
	if binaryVersion != 1 {
		t.Fatalf("binaryVersion = %d, want 1", binaryVersion)
	}
	raw, err := migrationsFS.ReadFile("migrations/00001_events.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"pos    INTEGER PRIMARY KEY",
		"UNIQUE (stream, seq)",
		"id     TEXT    NOT NULL UNIQUE",
		"length(data) BETWEEN 2 AND 1048576",
		"length(time) = 24",
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("migration lacks %q", want)
		}
	}
	if strings.Contains(strings.ToUpper(string(raw)), "WITHOUT ROWID") {
		t.Error("the events table must be an ordinary rowid table")
	}
}

// The schema's CHECKs back the validation: a row that breaks them is refused by SQLite.
func TestSchema_ChecksRejectMalformedRows(t *testing.T) {
	t.Parallel()
	l := openTemp(t, Options{})
	bad := map[string][]any{
		"short time": {"s", 1, "id-1", "t", "2026-01-01T00:00:00Z", []byte("{}")},
		"zero seq":   {"s", 0, "id-2", "t", "2026-01-01T00:00:00.000Z", []byte("{}")},
		"empty data": {"s", 1, "id-3", "t", "2026-01-01T00:00:00.000Z", []byte("")},
	}
	for name, args := range bad {
		if _, err := l.wdb.Exec(`INSERT INTO events (stream, seq, id, type, time, data) VALUES (?, ?, ?, ?, ?, ?)`, args...); err == nil {
			t.Errorf("%s: insert succeeded", name)
		}
	}
	mustAppend(t, l, NewEvent{Stream: "s", ID: "dup", Type: "t"})
	if _, err := l.wdb.Exec(`INSERT INTO events (stream, seq, id, type, time, data) VALUES ('s', 1, 'other', 't', '2026-01-01T00:00:00.000Z', x'7b7d')`); err == nil {
		t.Error("duplicate (stream, seq) was accepted")
	}
}

// UT-100
func TestAppend_RejectsInvalidTypeAndStream(t *testing.T) {
	t.Parallel()
	l := openTemp(t, Options{})
	ctx := context.Background()
	appendTicks(t, l, "s", 1)

	cases := []struct {
		name string
		e    NewEvent
		want error
	}{
		{"newline in type", NewEvent{Stream: "s", Type: "daemon\nstarted"}, ErrInvalidType},
		{"uppercase type", NewEvent{Stream: "s", Type: "Daemon"}, ErrInvalidType},
		{"empty type", NewEvent{Stream: "s", Type: ""}, ErrInvalidType},
		{"type too long", NewEvent{Stream: "s", Type: "a" + strings.Repeat("b", 128)}, ErrInvalidType},
		{"trailing newline in type", NewEvent{Stream: "s", Type: "daemon\n"}, ErrInvalidType},
		{"digit-first stream", NewEvent{Stream: "9x", Type: "t"}, ErrInvalidStream},
		{"empty stream", NewEvent{Stream: "", Type: "t"}, ErrInvalidStream},
		{"stream with slash", NewEvent{Stream: "a/b", Type: "t"}, ErrInvalidStream},
		{"stream too long", NewEvent{Stream: "a" + strings.Repeat("b", 128), Type: "t"}, ErrInvalidStream},
		{"id with space", NewEvent{Stream: "s", Type: "t", ID: "has space"}, ErrInvalidID},
		{"id too long", NewEvent{Stream: "s", Type: "t", ID: strings.Repeat("a", 65)}, ErrInvalidID},
	}
	for _, c := range cases {
		ev, created, err := l.Append(ctx, c.e)
		if !errors.Is(err, c.want) || created {
			t.Errorf("%s: created=%v err=%v; want %v", c.name, created, err, c.want)
		}
		if ev.ID == "" {
			t.Errorf("%s: the returned event carries no ID", c.name)
		}
	}
	if head, _ := l.Head(ctx, "s"); head != 1 {
		t.Fatalf("Head(s) = %d after rejected appends, want 1", head)
	}
	for _, stream := range []string{"9x", "a/b", ""} {
		if head, _ := l.Head(ctx, stream); head != 0 {
			t.Fatalf("Head(%q) = %d, want 0", stream, head)
		}
	}
	if n := countRows(t, l, ""); n != 1 {
		t.Fatalf("rows = %d after rejected appends, want 1", n)
	}
}

func TestAppend_AcceptsBoundaryNames(t *testing.T) {
	t.Parallel()
	l := openTemp(t, Options{})
	for _, e := range []NewEvent{
		{Stream: "a" + strings.Repeat("B", 127), Type: "a" + strings.Repeat("b", 127)},
		{Stream: "session:01ARZ3NDEKTSV4RRFFQ69G5FAV", Type: "daemon.unclean_shutdown_detected", ID: strings.Repeat("Z", 64)},
		{Stream: "x", Type: "a:b-c.d_e", ID: "A.b_c:d-e"},
	} {
		mustAppend(t, l, e)
	}
}

// UT-101
func TestAppend_ValidatesData(t *testing.T) {
	t.Parallel()
	l := openTemp(t, Options{})
	ctx := context.Background()

	for _, bad := range []string{`{"a":`, `not json`, `{"a":1} {"b":2}`, `1`} {
		ev, created, err := l.Append(ctx, NewEvent{Stream: "s", Type: "t", Data: []byte(bad)})
		if !errors.Is(err, ErrInvalidData) || created || ev.ID == "" {
			t.Errorf("data %q: created=%v id=%q err=%v; want ErrInvalidData with an ID", bad, created, ev.ID, err)
		}
	}
	if head, _ := l.Head(ctx, "s"); head != 0 {
		t.Fatalf("Head = %d after rejected data, want 0", head)
	}

	ev := mustAppend(t, l, NewEvent{Stream: "s", Type: "t"}) // nil data
	if string(ev.Data) != "{}" {
		t.Fatalf("nil data returned %q, want {}", ev.Data)
	}
	stored, ok, err := l.Last(ctx, "s")
	if err != nil || !ok || string(stored.Data) != "{}" {
		t.Fatalf("stored data = %q, %v, %v; want {}", stored.Data, ok, err)
	}
	// Any JSON value of two bytes or more is data.
	for _, ok := range []string{`null`, `[]`, `"x"`, `10`, `true`, ` {"a":1}`} {
		mustAppend(t, l, NewEvent{Stream: "s", Type: "t", Data: []byte(ok)})
	}
}

func TestAppend_GeneratesAULIDWhenNoIDIsGiven(t *testing.T) {
	t.Parallel()
	l := openTemp(t, Options{})
	ulidShape := regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		ev := mustAppend(t, l, NewEvent{Stream: "s", Type: "t"})
		if !ulidShape.MatchString(ev.ID) || seen[ev.ID] {
			t.Fatalf("event ID %q is not a fresh ULID", ev.ID)
		}
		seen[ev.ID] = true
	}
	// A producer-supplied ID is kept as given.
	if ev := mustAppend(t, l, NewEvent{Stream: "s", Type: "t", ID: "mine-1"}); ev.ID != "mine-1" {
		t.Fatalf("ID = %q, want mine-1", ev.ID)
	}
}

func TestAppend_CopiesTheCallersData(t *testing.T) {
	t.Parallel()
	l := openTemp(t, Options{})
	data := []byte(`{"a":1}`)
	ev := mustAppend(t, l, NewEvent{Stream: "s", Type: "t", Data: data})
	copy(data, `{"a":2}`)
	if string(ev.Data) != `{"a":1}` {
		t.Fatalf("returned data aliases the caller's slice: %s", ev.Data)
	}
}

// UT-102
func TestAppend_SameIDWithDifferentContentIsAConflict(t *testing.T) {
	t.Parallel()
	var commits atomic.Int32
	l := openTemp(t, Options{OnCommit: func(Event) { commits.Add(1) }})
	ctx := context.Background()
	stored := mustAppend(t, l, NewEvent{Stream: "s", ID: "X", Type: "thing.done", Data: []byte(`{"a":1}`)})

	for name, e := range map[string]NewEvent{
		"different data":   {Stream: "s", ID: "X", Type: "thing.done", Data: []byte(`{"a":2}`)},
		"different stream": {Stream: "other", ID: "X", Type: "thing.done", Data: []byte(`{"a":1}`)},
		"different type":   {Stream: "s", ID: "X", Type: "thing.undone", Data: []byte(`{"a":1}`)},
		"formatting only":  {Stream: "s", ID: "X", Type: "thing.done", Data: []byte(`{"a": 1}`)},
	} {
		ev, created, err := l.Append(ctx, e)
		if !errors.Is(err, ErrIDConflict) || created {
			t.Errorf("%s: created=%v err=%v; want ErrIDConflict", name, created, err)
		}
		if ev.ID != "X" || ev.Seq == stored.Seq || ev.Pos == stored.Pos || len(ev.Data) != 0 {
			t.Errorf("%s: returned %+v looks like the stored event; want only the ID", name, ev)
		}
	}
	if n := countRows(t, l, ""); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
	if head, _ := l.Head(ctx, "s"); head != 1 {
		t.Fatalf("Head(s) = %d, want 1", head)
	}
	if head, _ := l.Head(ctx, "other"); head != 0 {
		t.Fatalf("Head(other) = %d, want 0", head)
	}
	if n := commits.Load(); n != 1 {
		t.Fatalf("OnCommit called %d times, want 1", n)
	}
}

// UT-103
func TestClose_LaterAppendsReturnErrClosed(t *testing.T) {
	t.Parallel()
	path := dbPath(t)
	l := openAt(t, path, Options{})
	appendTicks(t, l, "s", 1)
	if err := l.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ev, created, err := l.Append(context.Background(), NewEvent{Stream: "s", Type: "t"})
	if !errors.Is(err, ErrClosed) || created || ev.ID == "" {
		t.Fatalf("Append after Close: created=%v id=%q err=%v; want ErrClosed with an ID", created, ev.ID, err)
	}
	if _, err := l.Read(context.Background(), "s", 0, 10); !errors.Is(err, ErrClosed) {
		t.Fatalf("Read after Close: %v, want ErrClosed", err)
	}
	if _, err := l.Head(context.Background(), "s"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Head after Close: %v, want ErrClosed", err)
	}
	if err := l.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	// The rejected append left nothing behind.
	again := openAt(t, path, Options{})
	if head, _ := again.Head(context.Background(), "s"); head != 1 {
		t.Fatalf("Head after reopen = %d, want 1", head)
	}
}

// waitFor polls cond until it holds or the test times out.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// UT-104
func TestAppend_FullQueueBlocksUntilTheCallersDeadline(t *testing.T) {
	t.Parallel()
	gate := make(chan struct{})
	l := openTemp(t, Options{hooks: hooks{beforeDequeue: func() { <-gate }}})
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < queueSize; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := l.Append(ctx, NewEvent{Stream: "s", Type: "tick"}); err != nil {
				t.Errorf("queued Append: %v", err)
			}
		}()
	}
	waitFor(t, "the queue to fill", func() bool { return len(l.queue) == queueSize })

	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	ev, created, err := l.Append(short, NewEvent{Stream: "s", Type: "tick"})
	if !errors.Is(err, context.DeadlineExceeded) || created {
		t.Fatalf("257th Append: created=%v err=%v; want DeadlineExceeded", created, err)
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Fatalf("257th Append returned after %v; it should have blocked until its deadline", elapsed)
	}
	if ev.ID == "" {
		t.Fatal("the returned event carries no generated ID")
	}

	close(gate)
	wg.Wait()
	waitFor(t, "the writer to drain", func() bool { return len(l.queue) == 0 })
	if n := countRows(t, l, "id = ?", ev.ID); n != 0 {
		t.Fatalf("the timed-out append was written (%d rows with ID %s)", n, ev.ID)
	}
	if head, _ := l.Head(ctx, "s"); head != queueSize {
		t.Fatalf("Head = %d, want %d", head, queueSize)
	}
}

func TestAppend_AlreadyCancelledContextEnqueuesNothing(t *testing.T) {
	t.Parallel()
	l := openTemp(t, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 50; i++ {
		ev, _, err := l.Append(ctx, NewEvent{Stream: "s", Type: "tick"})
		if !errors.Is(err, context.Canceled) || ev.ID == "" {
			t.Fatalf("Append with a cancelled ctx: id=%q err=%v", ev.ID, err)
		}
	}
	if n := countRows(t, l, ""); n != 0 {
		t.Fatalf("rows = %d, want 0", n)
	}
}

// UT-105
func TestAppend_CancelledCallerCannotAbortACommitInProgress(t *testing.T) {
	t.Parallel()
	var (
		once    sync.Once
		entered = make(chan struct{})
		release = make(chan struct{})
		commits atomic.Int32
	)
	l := openTemp(t, Options{
		OnCommit: func(Event) { commits.Add(1) },
		hooks: hooks{inTx: func(string) {
			once.Do(func() { close(entered); <-release })
		}},
	})

	ctx, cancel := context.WithCancel(context.Background())
	type outcome struct {
		ev      Event
		created bool
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		ev, created, err := l.Append(ctx, NewEvent{Stream: "s", Type: "tick", Data: []byte(`{"k":"v"}`)})
		done <- outcome{ev, created, err}
	}()

	<-entered // the writer is inside the transaction, after the INSERT
	cancel()
	got := <-done
	if !errors.Is(got.err, context.Canceled) || got.created {
		t.Fatalf("cancelled Append: created=%v err=%v; want context.Canceled", got.created, got.err)
	}
	if got.ev.ID == "" {
		t.Fatal("the cancelled Append returned no ID")
	}

	close(release)
	waitFor(t, "the commit to complete", func() bool {
		head, err := l.Head(context.Background(), "s")
		return err == nil && head == 1
	})
	waitFor(t, "OnCommit", func() bool { return commits.Load() == 1 })

	// A retry with the returned ID finds the committed event.
	retry, created, err := l.Append(context.Background(), NewEvent{Stream: "s", ID: got.ev.ID, Type: "tick", Data: []byte(`{"k":"v"}`)})
	if err != nil || created {
		t.Fatalf("retry: created=%v err=%v; want created=false", created, err)
	}
	if retry.Seq != 1 || retry.ID != got.ev.ID {
		t.Fatalf("retry = %+v, want seq 1 with ID %s", retry, got.ev.ID)
	}
	if n := countRows(t, l, ""); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
	if n := commits.Load(); n != 1 {
		t.Fatalf("OnCommit called %d times, want 1", n)
	}
}

func TestClose_DrainsQueuedAppendsAndRefusesNewOnes(t *testing.T) {
	t.Parallel()
	path := dbPath(t)
	gate := make(chan struct{})
	l := openAt(t, path, Options{hooks: hooks{beforeDequeue: func() { <-gate }}})
	ctx := context.Background()

	const queued = 10
	var wg sync.WaitGroup
	for i := 0; i < queued; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, created, err := l.Append(ctx, NewEvent{Stream: "s", Type: "tick"}); err != nil || !created {
				t.Errorf("queued Append: created=%v err=%v", created, err)
			}
		}()
	}
	waitFor(t, "appends to queue", func() bool { return len(l.queue) == queued })

	closeErr := make(chan error, 1)
	go func() { closeErr <- l.Close(ctx) }()
	waitFor(t, "Close to begin", func() bool {
		select {
		case <-l.closing:
			return true
		default:
			return false
		}
	})
	if _, _, err := l.Append(ctx, NewEvent{Stream: "s", Type: "late"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Append during Close: %v, want ErrClosed", err)
	}
	select {
	case err := <-closeErr:
		t.Fatalf("Close returned before the queue drained: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(gate)
	if err := <-closeErr; err != nil {
		t.Fatalf("Close: %v", err)
	}
	wg.Wait()

	again := openAt(t, path, Options{})
	if head, _ := again.Head(ctx, "s"); head != queued {
		t.Fatalf("Head after drain = %d, want %d", head, queued)
	}
}

func TestClose_ConcurrentCallsAllReturnTheFirstResult(t *testing.T) {
	t.Parallel()
	l := openTemp(t, Options{})
	appendTicks(t, l, "s", 2)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.Close(context.Background()); err != nil {
				t.Errorf("Close: %v", err)
			}
		}()
	}
	wg.Wait()
}

func TestClose_ContextExpiryAbortsTheDrainAndFailsQueuedAppends(t *testing.T) {
	t.Parallel()
	gate := make(chan struct{})
	l := openTemp(t, Options{hooks: hooks{beforeDequeue: func() { <-gate }}})

	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := l.Append(context.Background(), NewEvent{Stream: "s", Type: "tick"})
			errs <- err
		}()
	}
	waitFor(t, "appends to queue", func() bool { return len(l.queue) == 3 })

	time.AfterFunc(150*time.Millisecond, func() { close(gate) })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := l.Close(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close = %v, want a DeadlineExceeded error", err)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if !errors.Is(err, ErrClosed) {
			t.Errorf("queued Append after a forced Close: %v, want ErrClosed", err)
		}
	}
}

// UT-109: a Close whose ctx has ended returns at once while another Close is still finishing, so
// its return says nothing about the connections. Closed is the signal that they are shut:
// it stays open until the writer has stopped and the files are closed, and then the WAL is
// gone.
func TestClosed_WaitsForTheCloseAnotherCallerStarted(t *testing.T) {
	t.Parallel()
	path := dbPath(t)
	gate := make(chan struct{})
	l := openAt(t, path, Options{hooks: hooks{beforeDequeue: func() { <-gate }}})
	select {
	case <-l.Closed():
		t.Fatal("Closed is closed on an open ledger")
	default:
	}

	// The first Close runs out of time while the writer is held, so it is still inside
	// shutdown, waiting for the writer, when the second one comes.
	firstCtx, cancelFirst := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelFirst()
	first := make(chan error, 1)
	go func() { first <- l.Close(firstCtx) }()
	waitFor(t, "the first Close to give up on the drain", func() bool { return l.life.Err() != nil })

	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.Close(expired); !errors.Is(err, context.Canceled) {
		t.Fatalf("second Close with an ended ctx = %v, want context.Canceled at once", err)
	}
	select {
	case <-l.Closed():
		t.Fatal("Closed is closed while the first Close is still waiting for the writer")
	default:
	}

	close(gate)
	select {
	case <-l.Closed():
	case <-time.After(10 * time.Second):
		t.Fatal("Closed did not close after the writer was released")
	}
	if err := <-first; !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("first Close = %v, want a DeadlineExceeded error", err)
	}
	if _, err := os.Stat(path + "-wal"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the WAL is still there once Closed is closed: %v", err)
	}
}

// UT-106
func TestRead_PageStopsAtTheByteBudget(t *testing.T) {
	t.Parallel()
	l := openTemp(t, Options{})
	ctx := context.Background()
	data := jsonString(1 << 20) // exactly 1 MiB
	for i := 0; i < 20; i++ {
		mustAppend(t, l, NewEvent{Stream: "big", Type: "blob", Data: data})
	}

	page, err := l.Read(ctx, "big", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 8 || !page.HasMore || page.Events[0].Seq != 1 || page.Events[7].Seq != 8 {
		t.Fatalf("first page: %d events, HasMore=%v; want seqs 1..8 with HasMore", len(page.Events), page.HasMore)
	}
	page, err = l.Read(ctx, "big", 8, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 8 || !page.HasMore || page.Events[0].Seq != 9 {
		t.Fatalf("second page: %d events, HasMore=%v; want seqs 9..16 with HasMore", len(page.Events), page.HasMore)
	}
	page, err = l.Read(ctx, "big", 16, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 4 || page.HasMore {
		t.Fatalf("third page: %d events, HasMore=%v; want 4 without HasMore", len(page.Events), page.HasMore)
	}
	// The row limit still wins when it is the tighter bound.
	page, err = l.Read(ctx, "big", 0, 3)
	if err != nil || len(page.Events) != 3 || !page.HasMore {
		t.Fatalf("limit page: %d events, HasMore=%v, %v", len(page.Events), page.HasMore, err)
	}
}

func TestRead_ABudgetSmallerThanOneEventStillReturnsOne(t *testing.T) {
	t.Parallel()
	l := openTemp(t, Options{})
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		mustAppend(t, l, NewEvent{Stream: "s", Type: "blob", Data: jsonString(1 << 20)})
	}
	page, err := l.readBudget(ctx, "s", 0, 10, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].Seq != 1 || !page.HasMore {
		t.Fatalf("page = %d events, HasMore=%v; want exactly the first event with HasMore", len(page.Events), page.HasMore)
	}
	page, err = l.readBudget(ctx, "s", 2, 10, 1)
	if err != nil || len(page.Events) != 1 || page.Events[0].Seq != 3 || page.HasMore {
		t.Fatalf("last page = %+v, %v", page, err)
	}
}

var fixedWidthTime = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`)

// storedTimes returns every stored time in pos order.
func storedTimes(t *testing.T, l *Ledger) []string {
	t.Helper()
	rows, err := l.rdb.Query(`SELECT time FROM events ORDER BY pos`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// UT-107
func TestAppend_TimesNeverDecreaseEvenWhenTheClockGoesBackwards(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	// Offsets relative to base: forward, back, back further, forward, sub-millisecond noise.
	offsets := []time.Duration{0, 5 * time.Second, -time.Hour, 2 * time.Second, 5*time.Second + 400*time.Microsecond, 7 * time.Second}
	var now time.Time
	path := dbPath(t)
	open := func() *Ledger { return openAt(t, path, Options{clock: func() time.Time { return now }}) }

	l := open()
	for _, off := range offsets {
		now = base.Add(off)
		mustAppend(t, l, NewEvent{Stream: "s", Type: "tick"})
	}
	if err := l.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	// After a restart the clock is far in the past: the floor comes from the last row.
	l = open()
	for _, off := range []time.Duration{-24 * time.Hour, -48 * time.Hour} {
		now = base.Add(off)
		mustAppend(t, l, NewEvent{Stream: "s", Type: "tick"})
	}

	times := storedTimes(t, l)
	if len(times) != len(offsets)+2 {
		t.Fatalf("stored %d times, want %d", len(times), len(offsets)+2)
	}
	for i, s := range times {
		if len(s) != 24 || !fixedWidthTime.MatchString(s) {
			t.Fatalf("time[%d] = %q is not fixed width", i, s)
		}
		if i > 0 && s < times[i-1] {
			t.Fatalf("time[%d] = %s is before time[%d] = %s", i, s, i-1, times[i-1])
		}
	}
	sorted := append([]string(nil), times...)
	sort.Strings(sorted)
	for i := range times {
		if times[i] != sorted[i] {
			t.Fatalf("lexicographic order differs from pos order at %d: %v", i, times)
		}
	}
	want := []string{
		"2026-10-28T12:00:00.000Z", "2026-10-28T12:00:05.000Z", "2026-10-28T12:00:05.000Z", "2026-10-28T12:00:05.000Z",
		"2026-10-28T12:00:05.000Z", "2026-10-28T12:00:07.000Z", "2026-10-28T12:00:07.000Z", "2026-10-28T12:00:07.000Z",
	}
	for i := range want {
		if times[i] != want[i] {
			t.Fatalf("times = %v, want %v", times, want)
		}
	}
}

func TestAppend_TimeIsTruncatedToMillisecondsInUTC(t *testing.T) {
	t.Parallel()
	zone := time.FixedZone("UTC+3", 3*3600)
	at := time.Date(2026, 10, 28, 15, 4, 5, 123_999_999, zone)
	l := openTemp(t, Options{clock: func() time.Time { return at }})
	ev := mustAppend(t, l, NewEvent{Stream: "s", Type: "tick"})
	if got := ev.Time.String(); got != "2026-10-28T12:04:05.123Z" {
		t.Fatalf("time = %s, want 2026-10-28T12:04:05.123Z", got)
	}
	stored, _, _ := l.Last(context.Background(), "s")
	if !stored.Time.Equal(ev.Time.Time) {
		t.Fatalf("stored time %v != returned %v", stored.Time, ev.Time)
	}
}

func TestEvent_JSONShape(t *testing.T) {
	t.Parallel()
	ev := Event{
		Pos: 7, Stream: "daemon", Seq: 2, ID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Type: "daemon.started",
		Time: Time{time.Date(2026, 10, 28, 22, 10, 3, 140_000_000, time.UTC)},
		Data: json.RawMessage(`{"pid":48211}`),
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"stream":"daemon","seq":2,"id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","type":"daemon.started","time":"2026-10-28T22:10:03.140Z","data":{"pid":48211}}`
	if string(raw) != want {
		t.Fatalf("JSON = %s\nwant   %s", raw, want)
	}
	var back Event
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Pos != 0 || back.Seq != 2 || !back.Time.Equal(ev.Time.Time) || string(back.Data) != `{"pid":48211}` {
		t.Fatalf("round trip = %+v", back)
	}
	for _, bad := range []string{`"2026-10-28T22:10:03Z"`, `"2026-10-28 22:10:03.140"`, `12`} {
		var tm Time
		if err := json.Unmarshal([]byte(bad), &tm); err == nil {
			t.Errorf("Time accepted %s", bad)
		}
	}
}

// UT-108
func TestAppend_PosFollowsCommitOrderAcrossStreams(t *testing.T) {
	t.Parallel()
	var (
		mu    sync.Mutex
		order []Event
	)
	l := openTemp(t, Options{OnCommit: func(ev Event) {
		mu.Lock()
		order = append(order, ev)
		mu.Unlock()
	}})

	// Sequential interleaving.
	var last int64
	for i := 0; i < 10; i++ {
		stream := []string{"a", "b"}[i%2]
		ev := mustAppend(t, l, NewEvent{Stream: stream, Type: "tick"})
		if ev.Pos <= last {
			t.Fatalf("pos %d after %d: not strictly increasing", ev.Pos, last)
		}
		last = ev.Pos
	}

	// Concurrent producers on both streams.
	var wg sync.WaitGroup
	for _, stream := range []string{"a", "b", "a", "b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				mustAppend(t, l, NewEvent{Stream: stream, Type: "tick"})
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 110 {
		t.Fatalf("OnCommit saw %d events, want 110", len(order))
	}
	for i := 1; i < len(order); i++ {
		if order[i].Pos <= order[i-1].Pos {
			t.Fatalf("commit order %d has pos %d after pos %d", i, order[i].Pos, order[i-1].Pos)
		}
	}
	// The stored rows agree: pos order is commit order, and seq rises within each stream.
	rows, err := l.rdb.Query(`SELECT pos, stream, seq FROM events ORDER BY pos`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seqs := map[string]int64{}
	i := 0
	for rows.Next() {
		var pos, seq int64
		var stream string
		if err := rows.Scan(&pos, &stream, &seq); err != nil {
			t.Fatal(err)
		}
		if pos != order[i].Pos || stream != order[i].Stream || seq != order[i].Seq {
			t.Fatalf("row %d = (%d, %s, %d); OnCommit saw (%d, %s, %d)", i, pos, stream, seq, order[i].Pos, order[i].Stream, order[i].Seq)
		}
		if seq != seqs[stream]+1 {
			t.Fatalf("stream %s: seq %d after %d", stream, seq, seqs[stream])
		}
		seqs[stream] = seq
		i++
	}
}

func TestInspect_ReportsIntegrityAndSchemaWithoutWriting(t *testing.T) {
	t.Parallel()
	path := dbPath(t)
	l := openAt(t, path, Options{})
	appendTicks(t, l, "s", 3)

	// Against a live ledger: Inspect reads the WAL too.
	got, err := Inspect(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if got.SchemaVersion != 1 || got.BinaryVersion != 1 || !got.IntegrityOK() || got.SchemaAhead() {
		t.Fatalf("Inspect = %+v, want schema 1/1 and quick_check ok", got)
	}
	if err := l.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	before, _ := os.ReadFile(path)
	if got, err = Inspect(context.Background(), path); err != nil || !got.IntegrityOK() {
		t.Fatalf("Inspect after close = %+v, %v", got, err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("Inspect changed the database file")
	}
}

func TestInspect_MissingFileIsAnErrorAndCreatesNothing(t *testing.T) {
	t.Parallel()
	path := dbPath(t)
	_, err := Inspect(context.Background(), path)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Inspect(missing) = %v, want an error wrapping os.ErrNotExist", err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 0 {
		t.Fatalf("Inspect created files: %v", entries)
	}
}

func TestInspect_AFileThatIsNotADatabaseIsAnError(t *testing.T) {
	t.Parallel()
	path := dbPath(t)
	if err := os.WriteFile(path, bytes.Repeat([]byte("not a sqlite database "), 400), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(context.Background(), path); err == nil {
		t.Fatal("Inspect accepted a file that is not a database")
	}
	if _, err := Open(context.Background(), Options{Path: path}); err == nil {
		t.Fatal("Open accepted a file that is not a database")
	}
}

func TestInspect_OnAnEmptyDatabaseReportsNoSchema(t *testing.T) {
	t.Parallel()
	path := dbPath(t)
	db, err := sql.Open(driverName, fileDSN(path, nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE unrelated (x)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	got, err := Inspect(context.Background(), path)
	if err != nil || got.SchemaVersion != 0 || !got.IntegrityOK() {
		t.Fatalf("Inspect = %+v, %v; want schema 0 and ok", got, err)
	}
}

func TestInspect_AfterACleanCloseLeavesNoFilesBehind(t *testing.T) {
	t.Parallel()
	path := dbPath(t)
	l := openAt(t, path, Options{})
	appendTicks(t, l, "s", 3)
	if err := l.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	before := dirNames(t, dir)

	got, err := Inspect(context.Background(), path)
	if err != nil || !got.IntegrityOK() || got.SchemaVersion != 1 {
		t.Fatalf("Inspect = %+v, %v; want schema 1 and quick_check ok", got, err)
	}
	if after := dirNames(t, dir); !slices.Equal(before, after) {
		t.Fatalf("Inspect changed the directory: before %v, after %v", before, after)
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
