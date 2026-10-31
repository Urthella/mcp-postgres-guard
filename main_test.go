package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// connect runs the real server over in-memory transports and returns a client session.
func connect(t *testing.T, g *Guard) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	serverSide, clientSide := mcp.NewInMemoryTransports()
	if _, err := newServer(g).Connect(ctx, serverSide, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, clientSide, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func callText(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return res.Content[0].(*mcp.TextContent).Text, res.IsError
}

func TestMCPTools(t *testing.T) {
	g, _ := testGuard(t, 2, 5*time.Second)
	cs := connect(t, g)

	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		if !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s is not marked read-only", tool.Name)
		}
		names = append(names, tool.Name)
	}
	if got := strings.Join(names, ","); got != "describe_table,explain,list_tables,query" {
		t.Fatalf("tools: %s", got)
	}

	out, isErr := callText(t, cs, "query", map[string]any{"sql": "select id, email from users order by id"})
	if isErr || !strings.Contains(out, "| 1 | user1@example.com |") || !strings.Contains(out, "Showing the first 2 rows") {
		t.Fatalf("query output (isError=%v):\n%s", isErr, out)
	}

	out, isErr = callText(t, cs, "query", map[string]any{"sql": "delete from users"})
	if !isErr || !strings.Contains(out, "only read statements are allowed") {
		t.Fatalf("delete should be a tool error, got isError=%v: %s", isErr, out)
	}

	out, isErr = callText(t, cs, "describe_table", map[string]any{"table": "users"})
	if isErr || !strings.Contains(out, "| email | text | not null |") {
		t.Fatalf("describe_table (isError=%v):\n%s", isErr, out)
	}
}

func TestListTables(t *testing.T) {
	g, _ := testGuard(t, 100, 5*time.Second)
	res, err := g.ListTables(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Markdown(), "| public | users | table |") {
		t.Fatalf("users missing:\n%s", res.Markdown())
	}
}

func TestDescribeTable(t *testing.T) {
	g, admin := testGuard(t, 100, 5*time.Second)
	out, err := g.DescribeTable(context.Background(), "public.users")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"| id | integer | not null | nextval('users_id_seq'::regclass) |",
		"| created_at | timestamp with time zone | not null | now() |",
		"PRIMARY KEY (id)",
		"UNIQUE (email)",
		"CREATE UNIQUE INDEX users_email_key ON public.users USING btree (email)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if _, err := g.DescribeTable(context.Background(), "nope"); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("unknown table: %v", err)
	}
	// The name is a bound parameter, so SQL in it is just an invalid name.
	if _, err := g.DescribeTable(context.Background(), "users; drop table users"); err == nil {
		t.Error("injection attempt accepted")
	}
	if n := userCount(t, admin); n != 250 {
		t.Fatalf("users changed: %d", n)
	}
}

func TestExplain(t *testing.T) {
	g, _ := testGuard(t, 100, 5*time.Second)
	plan, err := g.Explain(context.Background(), "SELECT * FROM users WHERE email = 'user7@example.com'", false)
	if err != nil || !strings.Contains(plan, "users") || strings.Contains(plan, "actual time") {
		t.Fatalf("plain explain: %v\n%s", err, plan)
	}
	plan, err = g.Explain(context.Background(), "SELECT count(*) FROM users", true)
	if err != nil || !strings.Contains(plan, "actual time") {
		t.Fatalf("explain analyze: %v\n%s", err, plan)
	}
	if _, err := g.Explain(context.Background(), "DELETE FROM users", true); err == nil {
		t.Fatal("explain of a write was accepted")
	}
}
