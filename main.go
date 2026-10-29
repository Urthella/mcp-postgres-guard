// Command mcp-postgres-guard is an MCP server that lets AI agents query Postgres read-only,
// with row limits and timeouts.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const version = "0.1.0"

type queryInput struct {
	SQL string `json:"sql" jsonschema:"one read-only SQL statement: SELECT, WITH, VALUES, TABLE, SHOW or EXPLAIN"`
}

type describeInput struct {
	Table string `json:"table" jsonschema:"table or view name, optionally schema-qualified like public.users"`
}

type explainInput struct {
	SQL     string `json:"sql" jsonschema:"the SELECT statement to explain"`
	Analyze bool   `json:"analyze,omitempty" jsonschema:"also run it to get real timings and row counts (still read-only and time-limited)"`
}

func newServer(g *Guard) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "mcp-postgres-guard", Version: version}, nil)
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true}

	mcp.AddTool(s, &mcp.Tool{
		Name: "query",
		Description: fmt.Sprintf("Run one read-only SQL statement and get the rows as a Markdown table. "+
			"Writes are rejected. At most %d rows are returned and the statement is cancelled after %s, "+
			"so filter and aggregate in SQL instead of fetching whole tables.", g.RowLimit, g.Timeout),
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in queryInput) (*mcp.CallToolResult, any, error) {
		res, err := g.Query(ctx, in.SQL)
		if err != nil {
			return nil, nil, err
		}
		return text(res.Markdown()), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_tables",
		Description: "List the tables and views this connection can read, with Postgres' row estimates. Start here to learn the schema.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		res, err := g.ListTables(ctx)
		if err != nil {
			return nil, nil, err
		}
		return text(res.Markdown()), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "describe_table",
		Description: "Show a table's columns with types, nullability and defaults, plus its constraints (keys, foreign keys, checks) and indexes.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in describeInput) (*mcp.CallToolResult, any, error) {
		out, err := g.DescribeTable(ctx, in.Table)
		if err != nil {
			return nil, nil, err
		}
		return text(out), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "explain",
		Description: "Show the query plan for a SELECT, to check index use before running something heavy. Set analyze to run it and get real timings.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in explainInput) (*mcp.CallToolResult, any, error) {
		plan, err := g.Explain(ctx, in.SQL, in.Analyze)
		if err != nil {
			return nil, nil, err
		}
		return text(plan), nil, nil
	})

	return s
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func main() {
	log.SetFlags(0)
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		log.Fatal("mcp-postgres-guard: set DATABASE_URL, ideally for a role that can only SELECT")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	g, err := NewGuard(ctx, url, envInt("PG_GUARD_ROW_LIMIT", 100), envDuration("PG_GUARD_TIMEOUT", 5*time.Second))
	if err != nil {
		log.Fatalf("mcp-postgres-guard: %v", err)
	}
	defer g.Close()
	if err := newServer(g).Run(ctx, &mcp.StdioTransport{}); err != nil {
		log.Fatalf("mcp-postgres-guard: %v", err)
	}
}

func envInt(key string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(key)); err == nil && n > 0 {
		return n
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(key)); err == nil && d > 0 {
		return d
	}
	return def
}
