package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const maxCell = 200 // runes per cell, so one huge JSON column can't flood the agent's context

// Guard runs agent-supplied SQL with three independent safety layers:
// a keyword allowlist, a READ ONLY transaction enforced by Postgres, and a statement timeout.
// The real boundary is still the database role: connect with one that can only SELECT.
type Guard struct {
	pool     *pgxpool.Pool
	RowLimit int
	Timeout  time.Duration
}

type kind int

const (
	viaCursor kind = iota + 1 // SELECT-like: read through a cursor, so the server stops after RowLimit+1 rows
	direct                    // SHOW / EXPLAIN: small results, run as is
)

type Result struct {
	Columns   []string
	Rows      [][]string
	Truncated bool
}

func NewGuard(ctx context.Context, url string, rowLimit int, timeout time.Duration) (*Guard, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("DATABASE_URL: %w", err)
	}
	cfg.MaxConns = 4
	cfg.ConnConfig.RuntimeParams["application_name"] = "mcp-postgres-guard" // visible in pg_stat_activity
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connecting to Postgres: %w", err)
	}
	return &Guard{pool: pool, RowLimit: rowLimit, Timeout: timeout}, nil
}

func (g *Guard) Close() { g.pool.Close() }

// Query runs one read-only statement from the agent.
func (g *Guard) Query(ctx context.Context, sql string) (*Result, error) {
	k, sql, err := classify(sql)
	if err != nil {
		return nil, err
	}
	return g.run(ctx, k, sql)
}

func (g *Guard) run(ctx context.Context, k kind, sql string, params ...string) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, g.Timeout+2*time.Second) // client-side backstop
	defer cancel()
	tx, err := g.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck // read-only: there is nothing to keep

	// Every statement goes through pgconn.ExecParams, i.e. the extended protocol, where Postgres
	// rejects input with more than one command. pgx's Exec silently switches to the simple protocol
	// when there are no arguments, and there "select 1; commit; drop table x" commits the read-only
	// transaction and runs the DROP.
	conn := tx.Conn().PgConn()
	exec := func(sql string, params ...string) *pgconn.ResultReader {
		values := make([][]byte, len(params))
		for i, p := range params {
			values[i] = []byte(p)
		}
		// Text results: values come back exactly as psql prints them.
		return conn.ExecParams(ctx, sql, values, nil, nil, []int16{pgx.TextFormatCode})
	}
	if _, err := exec(fmt.Sprintf("SET LOCAL statement_timeout = %d", g.Timeout.Milliseconds())).Close(); err != nil {
		return nil, err
	}
	if k == viaCursor {
		if _, err := exec("DECLARE guard NO SCROLL CURSOR FOR "+sql, params...).Close(); err != nil {
			return nil, err
		}
		sql, params = fmt.Sprintf("FETCH %d FROM guard", g.RowLimit+1), nil
	}

	rr := exec(sql, params...)
	res := &Result{}
	for _, f := range rr.FieldDescriptions() {
		res.Columns = append(res.Columns, f.Name)
	}
	for rr.NextRow() {
		if len(res.Rows) == g.RowLimit {
			res.Truncated = true
			continue
		}
		values := rr.Values()
		row := make([]string, len(values))
		for i, v := range values {
			if v == nil {
				row[i] = "NULL"
			} else {
				row[i] = string(v) // copies: Values is reused by the next row
			}
		}
		res.Rows = append(res.Rows, row)
	}
	if _, err := rr.Close(); err != nil {
		return nil, err
	}
	return res, nil
}

// ListTables lists the tables and views this role can read, with Postgres' row estimates.
func (g *Guard) ListTables(ctx context.Context) (*Result, error) {
	return g.run(ctx, direct, `
		SELECT n.nspname AS schema, c.relname AS name,
		       CASE c.relkind WHEN 'r' THEN 'table' WHEN 'p' THEN 'partitioned table' WHEN 'v' THEN 'view'
		                      WHEN 'm' THEN 'materialized view' WHEN 'f' THEN 'foreign table' END AS kind,
		       CASE WHEN c.relkind IN ('v', 'f') OR c.reltuples < 0 THEN '' ELSE c.reltuples::bigint::text END AS estimated_rows
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r', 'p', 'v', 'm', 'f') AND NOT c.relispartition
		  AND n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg_toast%'
		  AND has_table_privilege(c.oid, 'SELECT')
		ORDER BY 1, 2`)
}

// DescribeTable shows columns, constraints and indexes. The name is a bound parameter cast to
// regclass, so it resolves like it would in SQL (search_path, "Quoted" names) and can't inject.
func (g *Guard) DescribeTable(ctx context.Context, table string) (string, error) {
	sections := []struct{ title, sql string }{
		{"Columns", `
			SELECT a.attname AS column, format_type(a.atttypid, a.atttypmod) AS type,
			       CASE WHEN a.attnotnull THEN 'not null' ELSE '' END AS nullable,
			       coalesce(pg_get_expr(d.adbin, d.adrelid), '') AS "default"
			FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
			WHERE a.attrelid = $1::regclass AND a.attnum > 0 AND NOT a.attisdropped
			ORDER BY a.attnum`},
		{"Constraints", `
			SELECT conname AS name, pg_get_constraintdef(oid) AS definition
			FROM pg_constraint WHERE conrelid = $1::regclass ORDER BY contype, conname`},
		{"Indexes", `
			SELECT i.relname AS name, pg_get_indexdef(x.indexrelid) AS definition
			FROM pg_index x JOIN pg_class i ON i.oid = x.indexrelid
			WHERE x.indrelid = $1::regclass ORDER BY i.relname`},
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## %s\n", table)
	for _, s := range sections {
		res, err := g.run(ctx, direct, s.sql, table)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "\n### %s\n\n", s.title)
		if len(res.Rows) == 0 {
			b.WriteString("(none)\n")
			continue
		}
		b.WriteString(res.Markdown() + "\n")
	}
	return b.String(), nil
}

// Explain returns the query plan. With analyze the query really runs, still read-only and time-limited.
func (g *Guard) Explain(ctx context.Context, sql string, analyze bool) (string, error) {
	k, sql, err := classify(sql)
	if err != nil {
		return "", err
	}
	if k != viaCursor {
		return "", errors.New("explain takes a SELECT, WITH, VALUES or TABLE statement")
	}
	opts := "FORMAT TEXT"
	if analyze {
		opts = "ANALYZE, BUFFERS, FORMAT TEXT"
	}
	res, err := g.run(ctx, direct, "EXPLAIN ("+opts+") "+sql)
	if err != nil {
		return "", err
	}
	lines := make([]string, len(res.Rows))
	for i, r := range res.Rows {
		lines[i] = r[0]
	}
	plan := strings.Join(lines, "\n")
	if res.Truncated {
		plan += fmt.Sprintf("\n… plan cut after %d lines", len(lines))
	}
	return plan, nil
}

// classify allowlists read statements by their first keyword and trims trailing semicolons.
func classify(sql string) (kind, string, error) {
	sql = strings.TrimRight(strings.TrimSpace(sql), "; \t\r\n")
	switch kw := firstKeyword(sql); kw {
	case "select", "with", "values", "table":
		return viaCursor, sql, nil
	case "show", "explain":
		return direct, sql, nil
	case "":
		return 0, "", errors.New("empty query")
	default:
		return 0, "", fmt.Errorf("only read statements are allowed (SELECT, WITH, VALUES, TABLE, SHOW, EXPLAIN), got %s", strings.ToUpper(kw))
	}
}

// firstKeyword skips whitespace, opening parentheses, -- comments and (nested) /* */ comments.
func firstKeyword(sql string) string {
	for i := 0; i < len(sql); {
		switch c := sql[i]; {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '(':
			i++
		case strings.HasPrefix(sql[i:], "--"):
			nl := strings.IndexByte(sql[i:], '\n')
			if nl < 0 {
				return ""
			}
			i += nl + 1
		case strings.HasPrefix(sql[i:], "/*"):
			depth := 0
			for i < len(sql) {
				if strings.HasPrefix(sql[i:], "/*") {
					depth, i = depth+1, i+2
				} else if strings.HasPrefix(sql[i:], "*/") {
					depth, i = depth-1, i+2
					if depth == 0 {
						break
					}
				} else {
					i++
				}
			}
			if depth > 0 {
				return ""
			}
		default:
			j := i
			for j < len(sql) && isWordByte(sql[j]) {
				j++
			}
			if j == i {
				return sql[i : i+1]
			}
			return strings.ToLower(sql[i:j])
		}
	}
	return ""
}

func isWordByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// Markdown renders the result as a table an LLM reads well, with a note when rows were cut.
func (r *Result) Markdown() string {
	if len(r.Columns) == 0 {
		return "OK (no result set)."
	}
	var b strings.Builder
	line := func(cells []string) {
		b.WriteString("|")
		for _, c := range cells {
			b.WriteString(" " + cell(c) + " |")
		}
		b.WriteString("\n")
	}
	line(r.Columns)
	b.WriteString("|" + strings.Repeat("---|", len(r.Columns)) + "\n")
	for _, row := range r.Rows {
		line(row)
	}
	if r.Truncated {
		fmt.Fprintf(&b, "\nShowing the first %d rows; more exist. Narrow the query with WHERE or LIMIT.", len(r.Rows))
	} else {
		fmt.Fprintf(&b, "\n(%d row%s)", len(r.Rows), map[bool]string{true: "", false: "s"}[len(r.Rows) == 1])
	}
	return b.String()
}

func cell(s string) string {
	if utf8.RuneCountInString(s) > maxCell {
		s = string([]rune(s)[:maxCell]) + "…"
	}
	return strings.NewReplacer("|", `\|`, "\r\n", " ", "\n", " ").Replace(s)
}
