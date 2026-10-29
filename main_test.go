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

func TestMCPQueryTool(t *testing.T) {
	g, _ := testGuard(t, 2, 5*time.Second)
	cs := connect(t, g)

	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) == 0 || tools.Tools[0].Name != "query" || !tools.Tools[0].Annotations.ReadOnlyHint {
		t.Fatalf("tools: %+v", tools.Tools)
	}

	out, isErr := callText(t, cs, "query", map[string]any{"sql": "select id, email from users order by id"})
	if isErr || !strings.Contains(out, "| 1 | user1@example.com |") || !strings.Contains(out, "Showing the first 2 rows") {
		t.Fatalf("query output (isError=%v):\n%s", isErr, out)
	}

	out, isErr = callText(t, cs, "query", map[string]any{"sql": "delete from users"})
	if !isErr || !strings.Contains(out, "only read statements are allowed") {
		t.Fatalf("delete should be a tool error, got isError=%v: %s", isErr, out)
	}
}
