# mcp-postgres-guard

[![ci](https://github.com/Urthella/mcp-postgres-guard/actions/workflows/ci.yml/badge.svg)](https://github.com/Urthella/mcp-postgres-guard/actions/workflows/ci.yml)

An [MCP](https://modelcontextprotocol.io) server that lets AI agents query your Postgres **without being able to break it**: read-only, one statement at a time, time-limited and row-limited. A single Go binary.

```bash
go install github.com/Urthella/mcp-postgres-guard@latest
claude mcp add postgres -e DATABASE_URL="postgres://agent_ro:secret@localhost:5432/app" -- mcp-postgres-guard
```

Then ask the agent things like *"which customers placed more than 5 orders last month?"* and it writes and runs the SQL itself.

## Safety layers

Each layer stops something the others don't, and every one is covered by an integration test against a real Postgres.

| Layer | Stops |
|---|---|
| **Allowlist** on the first keyword (`SELECT`, `WITH`, `VALUES`, `TABLE`, `SHOW`, `EXPLAIN`), after skipping comments and parentheses | `INSERT`, `UPDATE`, `DELETE`, DDL, `SET`, `COPY`, `CALL`, `DO`, transaction control |
| **`READ ONLY` transaction**, enforced by Postgres | writes that pass the allowlist: `nextval()`, `SELECT ... FOR UPDATE`, `EXPLAIN ANALYZE DELETE ...` |
| **Server-side cursor** for row results | data-modifying CTEs (`WITH x AS (DELETE ...) SELECT ...`), and fetching more than `limit + 1` rows |
| **One statement only** (extended protocol) | `select 1; commit; drop table users`, which would end the read-only transaction and run the `DROP` |
| **`statement_timeout`** per query, plus a client-side deadline | runaway queries and lock waits |
| **Cell truncation** at 200 characters | one huge JSON column flooding the agent's context |

### The bug the tests caught

The multi-statement layer exists because of a real failure during development. [pgx](https://github.com/jackc/pgx) sends an `Exec` with no arguments over Postgres' *simple* protocol, even when you ask for the extended one, and the simple protocol runs every statement in the string. `select 1; commit; drop table users` committed the read-only transaction and dropped the table. Every statement now goes through `pgconn.ExecParams`, where Postgres parses the input as a single prepared statement and rejects it with `cannot insert multiple commands into a prepared statement`. [`TestMultipleStatementsCannotEscapeTheTransaction`](guard_test.go) keeps it that way.

### The real boundary is the role

These layers are guardrails. A read-only transaction can't stop functions with side effects outside the table data, like `pg_terminate_backend()` or `dblink`. Connect with a role that can only read:

```sql
CREATE ROLE agent_ro LOGIN PASSWORD 'secret';
GRANT pg_read_all_data TO agent_ro;                      -- PostgreSQL 14+
ALTER ROLE agent_ro SET default_transaction_read_only = on;
```

## Configuration

| Variable | Default | |
|---|---|---|
| `DATABASE_URL` | required | any libpq-style URL or DSN |
| `PG_GUARD_ROW_LIMIT` | `100` | rows returned per query |
| `PG_GUARD_TIMEOUT` | `5s` | per-statement timeout (Go duration) |

Connections show up as `application_name = mcp-postgres-guard` in `pg_stat_activity`.

**Cursor / Claude Desktop** (`mcp.json` / `claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "postgres": {
      "command": "mcp-postgres-guard",
      "env": { "DATABASE_URL": "postgres://agent_ro:secret@localhost:5432/app" }
    }
  }
}
```

## Tools

| Tool | |
|---|---|
| `query` | Run one read-only statement, results as a Markdown table, with a note when rows were cut |

## Develop

```bash
nix develop                     # Go, gopls, golangci-lint and PostgreSQL
TEST_DATABASE_URL=postgres://... go test ./...
```

Integration tests need `TEST_DATABASE_URL` pointing at a **disposable** database (they recreate a `users` table) and are skipped without it. CI runs them against PostgreSQL 14 and 18.

## License

[MIT](LICENSE)
