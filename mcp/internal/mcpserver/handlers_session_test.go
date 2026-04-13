package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/redis/go-redis/v9"

	"asynkor/mcp/config"
	"asynkor/mcp/internal/activity"
	"asynkor/mcp/internal/auth"
	"asynkor/mcp/internal/lease"
	"asynkor/mcp/internal/natsbus"
	"asynkor/mcp/internal/session"
	"asynkor/mcp/internal/teamctx"
	"asynkor/mcp/internal/work"
)

// newTestServer wires a *Server backed by miniredis and no-op nats/teamctx.
// It is deliberately minimal — the fake teamctx URL is unreachable, so
// PersistWork goroutines log an error and exit without affecting the test.
func newTestServer(t *testing.T) (*Server, func()) {
	t.Helper()

	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	srv := New(
		&config.Config{Port: "0"},
		auth.NewValidator("http://127.0.0.1:1", "unused"),
		session.NewStore(rc),
		work.NewStore(rc),
		lease.NewStore(rc),
		natsbus.New(""),
		activity.NewStore(rc),
		teamctx.NewStore("http://127.0.0.1:1", "unused"),
	)

	return srv, func() {
		_ = rc.Close()
		mr.Close()
	}
}

// ctxFor builds a handler context as if makeContextFunc had just run for a
// POST /message with the given stable sessID.
func ctxFor(sessID string) context.Context {
	team := &auth.TeamContext{
		TeamID:            "team-test",
		TeamSlug:          "test",
		HeartbeatInterval: 60,
		ConflictMode:      "warn",
	}
	ctx := context.Background()
	ctx = context.WithValue(ctx, ctxKeyTeam, team)
	ctx = context.WithValue(ctx, ctxKeySession, sessID)
	ctx = context.WithValue(ctx, ctxKeyHostname, "dev-mac")
	return ctx
}

func callStart(t *testing.T, srv *Server, ctx context.Context, plan string) *mcp.CallToolResult {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Name = "asynkor_start"
	req.Params.Arguments = map[string]any{"plan": plan}
	res, err := srv.handleStart(ctx, req)
	if err != nil {
		t.Fatalf("handleStart error: %v", err)
	}
	return res
}

func callFinish(t *testing.T, srv *Server, ctx context.Context, result string) *mcp.CallToolResult {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Name = "asynkor_finish"
	req.Params.Arguments = map[string]any{"result": result}
	res, err := srv.handleFinish(ctx, req)
	if err != nil {
		t.Fatalf("handleFinish error: %v", err)
	}
	return res
}

func resultText(t *testing.T, r *mcp.CallToolResult) string {
	t.Helper()
	if len(r.Content) == 0 {
		t.Fatal("empty tool result")
	}
	tc, ok := r.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", r.Content[0])
	}
	return tc.Text
}

// TestHandlerSession_StartFinishRoundTrip is the regression test for the bug
// where makeContextFunc generated a fresh UUID on every POST, causing
// asynkor_finish to fail with "no active work found" right after a
// successful asynkor_start. With a stable session ID in ctx, the round trip
// must succeed.
func TestHandlerSession_StartFinishRoundTrip(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	ctx := ctxFor("stable-sess-A")

	startRes := callStart(t, srv, ctx, "adding refresh token rotation")
	if startRes.IsError {
		t.Fatalf("start returned error: %s", resultText(t, startRes))
	}

	var startPayload map[string]any
	if err := json.Unmarshal([]byte(resultText(t, startRes)), &startPayload); err != nil {
		t.Fatalf("start payload not JSON: %v", err)
	}
	if startPayload["ok"] != true {
		t.Fatalf("start not ok: %v", startPayload)
	}
	if _, hasID := startPayload["work_id"].(string); !hasID {
		t.Fatalf("start missing work_id: %v", startPayload)
	}

	finishRes := callFinish(t, srv, ctx, "rotation landed, tests green")
	if finishRes.IsError {
		t.Fatalf("finish returned error with same session: %s", resultText(t, finishRes))
	}

	var finishPayload map[string]any
	if err := json.Unmarshal([]byte(resultText(t, finishRes)), &finishPayload); err != nil {
		t.Fatalf("finish payload not JSON: %v", err)
	}
	if finishPayload["ok"] != true {
		t.Fatalf("finish not ok: %v", finishPayload)
	}
}

// TestHandlerSession_FinishWithDifferentSessionFails locks in the intended
// isolation: work started under one session is not visible to a different
// session calling finish. Before the fix this was the *only* behaviour (every
// POST had a fresh UUID), which is why finish always failed.
func TestHandlerSession_FinishWithDifferentSessionFails(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	startCtx := ctxFor("sess-A")
	startRes := callStart(t, srv, startCtx, "working on frontend nav")
	if startRes.IsError {
		t.Fatalf("start returned error: %s", resultText(t, startRes))
	}

	finishCtx := ctxFor("sess-B") // different session — should not see sess-A's work
	finishRes := callFinish(t, srv, finishCtx, "trying to finish someone else's work")
	if !finishRes.IsError {
		t.Fatal("finish from a different session unexpectedly succeeded")
	}
	if msg := resultText(t, finishRes); !strings.Contains(msg, "no active work") {
		t.Errorf("unexpected error text: %q", msg)
	}
}

// TestHandlerSession_MultipleStartsSameSession verifies that re-calling start
// in the same session replaces the previous active work (the Lua startScript
// SREM's the old work ID from the active set). This is the "agent pivoted
// mid-work" case — it should not accumulate active entries.
func TestHandlerSession_MultipleStartsSameSession(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	ctx := ctxFor("sess-pivot")

	_ = callStart(t, srv, ctx, "first plan")
	_ = callStart(t, srv, ctx, "second plan after pivoting")

	active, err := srv.works.ListActive(ctx, "team-test")
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("expected 1 active work after pivot, got %d", len(active))
	}
	if active[0].Plan != "second plan after pivoting" {
		t.Errorf("wrong plan survived: %q", active[0].Plan)
	}
}
