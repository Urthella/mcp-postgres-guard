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
