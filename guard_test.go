package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestClassify(t *testing.T) {
	allowed := map[string]kind{
		"select 1":                                  viaCursor,
		"  SELECT * FROM users;  ":                  viaCursor,
		"with x as (select 1) select * from x":      viaCursor,
		"(select 1) union (select 2)":               viaCursor,
		"-- why\n/* outer /* nested */ */ select 1": viaCursor,
		"VALUES (1), (2)":                           viaCursor,
		"table users":                               viaCursor,
		"show work_mem":                             direct,
		"explain select 1":                          direct,
	}
	for sql, want := range allowed {
		if got, _, err := classify(sql); err != nil || got != want {
			t.Errorf("classify(%q) = %v, %v; want %v", sql, got, err, want)
		}
	}
	for _, sql := range []string{
		"insert into users values (1)", "UPDATE users SET a = 1", "delete from users", "drop table users",
		"truncate users", "set statement_timeout = 0", "copy users to '/tmp/x'", "call p()", "do $$ begin end $$",
		"begin", "commit", "grant all on users to public", "vacuum", "/* select */ delete from users", "", "  ;  ",
		"/* unterminated select 1",
	} {
		if _, _, err := classify(sql); err == nil {
			t.Errorf("classify(%q) allowed a non-read statement", sql)
		}
	}
	if _, sql, _ := classify("select 1 ;; \n"); sql != "select 1" {
		t.Errorf("trailing semicolons not trimmed: %q", sql)
	}
}

func TestCellEscapesAndTruncates(t *testing.T) {
	if got := cell("a|b\nc"); got != `a\|b c` {
		t.Errorf("cell = %q", got)
	}
	if got := cell(strings.Repeat("ş", maxCell+5)); !strings.HasSuffix(got, "…") || len([]rune(got)) != maxCell+1 {
		t.Errorf("long cell not truncated by runes: %d runes", len([]rune(got)))
	}
}

// testGuard needs TEST_DATABASE_URL (a disposable database: it recreates a "users" table).
func testGuard(t *testing.T, rowLimit int, timeout time.Duration) (*Guard, *pgx.Conn) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	_, err = admin.Exec(ctx, `
		DROP TABLE IF EXISTS users;
		CREATE TABLE users (id serial PRIMARY KEY, email text NOT NULL UNIQUE, created_at timestamptz NOT NULL DEFAULT now());
		INSERT INTO users (email) SELECT 'user' || i || '@example.com' FROM generate_series(1, 250) i;`)
	if err != nil {
		t.Fatal(err)
	}
	g, err := NewGuard(ctx, url, rowLimit, timeout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close(); _ = admin.Close(ctx) })
	return g, admin
}

func userCount(t *testing.T, admin *pgx.Conn) int {
	t.Helper()
	var n int
	if err := admin.QueryRow(context.Background(), "SELECT count(*) FROM users").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestQueryRowLimit(t *testing.T) {
	g, _ := testGuard(t, 100, 5*time.Second)
	res, err := g.Query(context.Background(), "SELECT id, email FROM users ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 100 || !res.Truncated {
		t.Fatalf("got %d rows, truncated=%v; want 100, true", len(res.Rows), res.Truncated)
	}
	if res.Rows[99][0] != "100" {
		t.Errorf("order lost through the cursor: last id %s", res.Rows[99][0])
	}
	res, err = g.Query(context.Background(), "select email from users where id <= 3")
	if err != nil || len(res.Rows) != 3 || res.Truncated {
		t.Fatalf("small query: %v rows, truncated=%v, err=%v", len(res.Rows), res.Truncated, err)
	}
}

// Statements that pass the keyword allowlist but still try to write.
func TestWritesInsideReadStatementsAreBlocked(t *testing.T) {
	g, admin := testGuard(t, 100, 5*time.Second)
	for sql, want := range map[string]string{
		// Postgres refuses data-modifying CTEs inside a cursor.
		"WITH gone AS (DELETE FROM users RETURNING 1) SELECT count(*) FROM gone": "data-modifying",
		// These reach the read-only transaction, which is what stops them.
		"SELECT nextval('users_id_seq')":    "read-only transaction",
		"SELECT * FROM users FOR UPDATE":    "read-only transaction",
		"EXPLAIN ANALYZE DELETE FROM users": "read-only transaction",
	} {
		if _, err := g.Query(context.Background(), sql); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want an error containing %q, got %v", sql, want, err)
		}
	}
	if n := userCount(t, admin); n != 250 {
		t.Fatalf("rows deleted: %d left", n)
	}
}

// Regression: pgx's Exec uses the simple protocol for argument-less SQL, where this input
// commits the read-only transaction and then runs the DROP. It must be rejected as a whole.
func TestMultipleStatementsCannotEscapeTheTransaction(t *testing.T) {
	g, admin := testGuard(t, 100, 5*time.Second)
	for _, sql := range []string{
		"select 1; commit; drop table users",
		"show work_mem; commit; delete from users",
	} {
		if _, err := g.Query(context.Background(), sql); err == nil || !strings.Contains(err.Error(), "multiple commands") {
			t.Errorf("%q: want a multiple-commands error, got %v", sql, err)
		}
		if n := userCount(t, admin); n != 250 {
			t.Fatalf("%q changed users: %d rows", sql, n)
		}
	}
}

func TestStatementTimeout(t *testing.T) {
	g, _ := testGuard(t, 100, 300*time.Millisecond)
	start := time.Now()
	_, err := g.Query(context.Background(), "SELECT pg_sleep(10)")
	if err == nil || !strings.Contains(err.Error(), "statement timeout") {
		t.Fatalf("expected a statement timeout, got %v", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("timeout took %s", took)
	}
}

func TestTextFormatAndNulls(t *testing.T) {
	g, _ := testGuard(t, 100, 5*time.Second)
	res, err := g.Query(context.Background(), "SELECT NULL::int AS a, 1.50::numeric AS b, 'x|y' AS c, '{1,2}'::int[] AS d")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(res.Rows[0], ","); got != "NULL,1.50,x|y,{1,2}" {
		t.Errorf("row = %s", got)
	}
	if md := res.Markdown(); !strings.Contains(md, `| NULL | 1.50 | x\|y | {1,2} |`) || !strings.HasSuffix(md, "(1 row)") {
		t.Errorf("markdown:\n%s", md)
	}
}

func TestShowAndExplainRunDirectly(t *testing.T) {
	g, _ := testGuard(t, 100, 5*time.Second)
	res, err := g.Query(context.Background(), "SHOW statement_timeout")
	if err != nil || res.Rows[0][0] != "5s" {
		t.Fatalf("SHOW inside the guard's transaction: %v %v", res, err)
	}
	res, err = g.Query(context.Background(), "EXPLAIN SELECT * FROM users WHERE id = 1")
	if err != nil || !strings.Contains(res.Rows[0][0], "users") {
		t.Fatalf("EXPLAIN: %v %v", res, err)
	}
}
