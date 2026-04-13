package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"asynkor/mcp/internal/auth"
)

func ctxForMode(sessID, mode string) context.Context {
	team := &auth.TeamContext{
		TeamID:            "team-test",
		TeamSlug:          "test",
		HeartbeatInterval: 60,
		ConflictMode:      mode,
	}
	ctx := context.Background()
	ctx = context.WithValue(ctx, ctxKeyTeam, team)
	ctx = context.WithValue(ctx, ctxKeySession, sessID)
	ctx = context.WithValue(ctx, ctxKeyHostname, "dev-mac")
	return ctx
}

func callStartWithArgs(t *testing.T, srv *Server, ctx context.Context, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Name = "asynkor_start"
	req.Params.Arguments = args
	res, err := srv.handleStart(ctx, req)
	if err != nil {
		t.Fatalf("handleStart error: %v", err)
	}
	return res
}

// TestOverlap_WarnModeSurfacesActiveTeammates verifies that a clean start
// (no overlap) still reports which teammates are active. This gives agents
// visibility into team state even when there's no direct conflict.
func TestOverlap_WarnModeSurfacesActiveTeammates(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	// Session A starts work on backend auth.
	ctxA := ctxForMode("sess-A", "warn")
	_ = callStartWithArgs(t, srv, ctxA, map[string]any{
		"plan":  "backend auth refactor",
		"paths": "backend/auth.go",
	})

	// Session B starts unrelated work on the frontend.
	ctxB := ctxForMode("sess-B", "warn")
	resB := callStartWithArgs(t, srv, ctxB, map[string]any{
		"plan":  "frontend landing hero copy tweak",
		"paths": "frontend/src/Hero.tsx",
	})
	if resB.IsError {
		t.Fatalf("unrelated start errored: %s", resultText(t, resB))
	}

	var body map[string]any
	_ = json.Unmarshal([]byte(resultText(t, resB)), &body)
	if body["ok"] != true {
		t.Fatalf("expected ok, got %v", body)
	}
	teammates, _ := body["active_teammates"].([]any)
	if len(teammates) != 1 {
		t.Fatalf("expected 1 active teammate, got %d", len(teammates))
	}
	if _, has := body["warnings"]; has {
		t.Errorf("unrelated start should not have warnings: %v", body["warnings"])
	}
}

// TestOverlap_WarnModeFlagsPathOverlap verifies that when two sessions
// declare overlapping paths, the second start succeeds (warn mode) but
// surfaces the overlap in warnings.active_overlap.
func TestOverlap_WarnModeFlagsPathOverlap(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	ctxA := ctxForMode("sess-A", "warn")
	_ = callStartWithArgs(t, srv, ctxA, map[string]any{
		"plan":  "first agent working on Nav",
		"paths": "frontend/src/Nav.tsx",
	})

	ctxB := ctxForMode("sess-B", "warn")
	resB := callStartWithArgs(t, srv, ctxB, map[string]any{
		"plan":  "second agent also touching Nav",
		"paths": "frontend/src/Nav.tsx,frontend/src/App.tsx",
	})
	if resB.IsError {
		t.Fatalf("warn-mode overlap should not error: %s", resultText(t, resB))
	}

	var body map[string]any
	_ = json.Unmarshal([]byte(resultText(t, resB)), &body)
	if body["ok"] != true {
		t.Fatalf("expected ok=true in warn mode, got %v", body)
	}
	warnings, ok := body["warnings"].(map[string]any)
	if !ok {
		t.Fatalf("expected warnings object, got %v", body["warnings"])
	}
	overlap, _ := warnings["active_overlap"].([]any)
	if len(overlap) != 1 {
		t.Fatalf("expected 1 overlap entry, got %d", len(overlap))
	}
	entry := overlap[0].(map[string]any)
	if reason, _ := entry["reason"].(string); !strings.Contains(reason, "Nav.tsx") {
		t.Errorf("reason should mention the overlapping path, got: %q", reason)
	}
}

// TestOverlap_PlanTextSimilarity verifies the fallback heuristic: when
// neither side provides paths, plan-text Jaccard similarity still flags
// obviously related work.
func TestOverlap_PlanTextSimilarity(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	ctxA := ctxForMode("sess-A", "warn")
	_ = callStartWithArgs(t, srv, ctxA, map[string]any{
		"plan": "adding refresh token rotation to authentication middleware",
	})

	ctxB := ctxForMode("sess-B", "warn")
	resB := callStartWithArgs(t, srv, ctxB, map[string]any{
		"plan": "refresh token rotation authentication fix",
	})

	var body map[string]any
	_ = json.Unmarshal([]byte(resultText(t, resB)), &body)
	warnings, _ := body["warnings"].(map[string]any)
	if warnings == nil {
		t.Fatalf("expected plan-text similarity to flag overlap, body=%v", body)
	}
	overlap, _ := warnings["active_overlap"].([]any)
	if len(overlap) != 1 {
		t.Fatalf("expected 1 overlap entry, got %d", len(overlap))
	}
	entry := overlap[0].(map[string]any)
	if reason, _ := entry["reason"].(string); !strings.Contains(reason, "similar plan text") {
		t.Errorf("reason should say similar plan text, got: %q", reason)
	}
}

// TestOverlap_PlanTextSimilarity_KeepsHighSignalVerbs locks in the audit
// fix: removing "add", "fix", "update", "change" from the stopword list
// means short engineering plans don't all collapse to ~0% Jaccard. Without
// the fix, plans like "fix jwt validation in auth middleware" and
// "fix jwt validation in auth handler" both stripped to a tiny set and
// missed an obvious overlap.
func TestOverlap_PlanTextSimilarity_KeepsHighSignalVerbs(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	ctxA := ctxForMode("sess-A", "warn")
	_ = callStartWithArgs(t, srv, ctxA, map[string]any{
		"plan": "fix jwt validation in auth middleware",
	})

	ctxB := ctxForMode("sess-B", "warn")
	resB := callStartWithArgs(t, srv, ctxB, map[string]any{
		"plan": "fix jwt validation in auth handler",
	})

	var body map[string]any
	_ = json.Unmarshal([]byte(resultText(t, resB)), &body)
	warnings, _ := body["warnings"].(map[string]any)
	overlap, _ := warnings["active_overlap"].([]any)
	if len(overlap) != 1 {
		t.Fatalf("expected 1 overlap entry after stopword tune, got %d (body=%v)", len(overlap), body)
	}
}

// TestOverlap_PlanTextSimilarity_UnrelatedShortPlansDoNotMatch verifies
// the other side of the stopword fix: two short plans whose only shared
// word is the verb ("add") with otherwise unrelated nouns must NOT score
// as overlap. Significant words: {database, migration, user, preferences}
// vs {typeahead, component, search}. Adding "add" to both gives Jaccard
// 1/8 = 0.125 < 0.3 — below threshold.
func TestOverlap_PlanTextSimilarity_UnrelatedShortPlansDoNotMatch(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	ctxA := ctxForMode("sess-A", "warn")
	_ = callStartWithArgs(t, srv, ctxA, map[string]any{
		"plan": "add database migration for user preferences",
	})

	ctxB := ctxForMode("sess-B", "warn")
	resB := callStartWithArgs(t, srv, ctxB, map[string]any{
		"plan": "add typeahead component to search bar",
	})

	var body map[string]any
	_ = json.Unmarshal([]byte(resultText(t, resB)), &body)
	if _, has := body["warnings"]; has {
		t.Errorf("unrelated short plans should not flag overlap, got warnings: %v", body["warnings"])
	}
}

// TestOverlap_BlockModeRefusesWithoutAck verifies that in block mode,
// an overlapping start is refused with ok=false and a conflicts payload.
func TestOverlap_BlockModeRefusesWithoutAck(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	ctxA := ctxForMode("sess-A", "block")
	_ = callStartWithArgs(t, srv, ctxA, map[string]any{
		"plan":  "first agent on auth",
		"paths": "src/auth.ts",
	})

	ctxB := ctxForMode("sess-B", "block")
	resB := callStartWithArgs(t, srv, ctxB, map[string]any{
		"plan":  "second agent also on auth",
		"paths": "src/auth.ts",
	})

	var body map[string]any
	_ = json.Unmarshal([]byte(resultText(t, resB)), &body)
	if body["ok"] != false {
		t.Fatalf("block mode should refuse overlap, got ok=%v", body["ok"])
	}
	if body["error"] != "overlap" {
		t.Errorf("expected error=overlap, got %v", body["error"])
	}
	conflicts, _ := body["conflicts"].([]any)
	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict entry, got %d", len(conflicts))
	}

	// Confirm the second work was NOT created.
	active, _ := srv.works.ListActive(context.Background(), "team-test")
	if len(active) != 1 {
		t.Fatalf("expected only the first work to be active, got %d", len(active))
	}
}

// TestOverlap_BlockModeAcknowledgeAllowsStart verifies the override path:
// passing acknowledge_overlap with every conflicting work_id lets the start
// proceed in block mode.
func TestOverlap_BlockModeAcknowledgeAllowsStart(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	ctxA := ctxForMode("sess-A", "block")
	resA := callStartWithArgs(t, srv, ctxA, map[string]any{
		"plan":  "first agent on auth",
		"paths": "src/auth.ts",
	})
	var bodyA map[string]any
	_ = json.Unmarshal([]byte(resultText(t, resA)), &bodyA)
	workAID, _ := bodyA["work_id"].(string)
	if workAID == "" {
		t.Fatal("expected first work_id in response")
	}

	ctxB := ctxForMode("sess-B", "block")
	resB := callStartWithArgs(t, srv, ctxB, map[string]any{
		"plan":                "second agent also on auth, coordinated with first",
		"paths":               "src/auth.ts",
		"acknowledge_overlap": workAID,
	})

	var bodyB map[string]any
	_ = json.Unmarshal([]byte(resultText(t, resB)), &bodyB)
	if bodyB["ok"] != true {
		t.Fatalf("acknowledged block should succeed: %v", bodyB)
	}
}

// TestOverlap_BlockModePartialAckStillBlocks verifies that acknowledging
// only one of two conflicting work_ids is not enough — the agent must
// acknowledge every conflict to proceed.
func TestOverlap_BlockModePartialAckStillBlocks(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	ctxA := ctxForMode("sess-A", "block")
	resA := callStartWithArgs(t, srv, ctxA, map[string]any{
		"plan":  "first agent on auth service",
		"paths": "src/auth.ts",
	})
	var bodyA map[string]any
	_ = json.Unmarshal([]byte(resultText(t, resA)), &bodyA)
	workAID, _ := bodyA["work_id"].(string)

	ctxB := ctxForMode("sess-B", "block")
	_ = callStartWithArgs(t, srv, ctxB, map[string]any{
		"plan":  "second agent on login middleware",
		"paths": "src/middleware.ts",
	})

	ctxC := ctxForMode("sess-C", "block")
	resC := callStartWithArgs(t, srv, ctxC, map[string]any{
		"plan":                "third agent wants both",
		"paths":               "src/auth.ts,src/middleware.ts",
		"acknowledge_overlap": workAID, // only acknowledges A, not B
	})
	var bodyC map[string]any
	_ = json.Unmarshal([]byte(resultText(t, resC)), &bodyC)
	if bodyC["ok"] != false {
		t.Fatalf("partial ack should still block, got ok=%v", bodyC["ok"])
	}
}

// TestOverlap_SameSessionDoesNotFlagItself verifies that calling
// asynkor_start again in the same session (a deliberate pivot) does not
// self-flag. Only OTHER sessions' active work counts.
func TestOverlap_SameSessionDoesNotFlagItself(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	ctx := ctxForMode("sess-pivot", "block")
	_ = callStartWithArgs(t, srv, ctx, map[string]any{
		"plan":  "initial plan on auth",
		"paths": "src/auth.ts",
	})

	res := callStartWithArgs(t, srv, ctx, map[string]any{
		"plan":  "pivoted: actually touching auth differently",
		"paths": "src/auth.ts",
	})
	var body map[string]any
	_ = json.Unmarshal([]byte(resultText(t, res)), &body)
	if body["ok"] != true {
		t.Fatalf("same-session pivot should succeed, got %v", body)
	}
}

// TestCheck_MatchesPlannedPaths verifies that asynkor_check now surfaces
// overlap against PlannedPaths too, not just FilesTouched. Before the fix,
// check was blind to in-flight work because FilesTouched is empty until
// asynkor_finish.
func TestCheck_MatchesPlannedPaths(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	// Agent A declares plans to touch Nav.tsx.
	ctxA := ctxForMode("sess-A", "warn")
	_ = callStartWithArgs(t, srv, ctxA, map[string]any{
		"plan":  "nav redesign",
		"paths": "frontend/src/Nav.tsx",
	})

	// Agent B calls check on Nav.tsx from a different session.
	ctxB := ctxForMode("sess-B", "warn")
	req := mcp.CallToolRequest{}
	req.Params.Name = "asynkor_check"
	req.Params.Arguments = map[string]any{"paths": "frontend/src/Nav.tsx"}
	res, err := srv.handleCheck(ctxB, req)
	if err != nil {
		t.Fatalf("handleCheck error: %v", err)
	}
	text := resultText(t, res)
	if !strings.Contains(text, "Active work on these files") {
		t.Errorf("check should surface active work on the planned path, got:\n%s", text)
	}
	if !strings.Contains(text, "nav redesign") {
		t.Errorf("check should mention the active plan, got:\n%s", text)
	}
}
