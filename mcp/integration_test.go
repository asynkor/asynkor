package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"asynkor/mcp/internal/auth"
	"asynkor/mcp/internal/session"
	"asynkor/mcp/internal/work"
)

// mockJavaServer simulates Java's /internal/validate-key endpoint.
func mockJavaServer(t *testing.T, token, teamID, teamSlug string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/validate-key" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("X-Internal-Token") != token {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			return
		}
		var body struct {
			APIKey string `json:"api_key"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.APIKey == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"team_id":   teamID,
			"team_slug": teamSlug,
			"plan":      "team",
			"config": map[string]any{
				"lease_ttl":           300,
				"heartbeat_interval":  60,
				"conflict_mode":       "warn",
				"ignore_patterns":     []string{"dist/*", "*.lock"},
				"allow_force_release": false,
			},
		})
	}))
}

func newTestRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return client, mr
}

// TestGoJavaIntegration verifies the full auth validation flow:
// Go calls Java's /internal/validate-key and correctly parses the team context.
func TestGoJavaIntegration_ValidateKey(t *testing.T) {
	java := mockJavaServer(t, "secret-token", "team-uuid-123", "acme")
	defer java.Close()

	v := auth.NewValidator(java.URL, "secret-token")

	ctx, err := v.Validate("cf_live_somekey")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if ctx.Teams[0].TeamID != "team-uuid-123" {
		t.Errorf("TeamID = %s, want team-uuid-123", ctx.Teams[0].TeamID)
	}
	if ctx.Teams[0].TeamSlug != "acme" {
		t.Errorf("TeamSlug = %s, want acme", ctx.Teams[0].TeamSlug)
	}
	if ctx.Teams[0].ConflictMode != "warn" {
		t.Errorf("ConflictMode = %s, want warn", ctx.Teams[0].ConflictMode)
	}
	if len(ctx.Teams[0].IgnorePatterns) != 2 {
		t.Errorf("IgnorePatterns = %v, want 2 items", ctx.Teams[0].IgnorePatterns)
	}
}

// TestGoJavaIntegration_BadToken verifies that wrong INTERNAL_TOKEN is rejected.
func TestGoJavaIntegration_BadToken(t *testing.T) {
	java := mockJavaServer(t, "correct-token", "id", "slug")
	defer java.Close()

	v := auth.NewValidator(java.URL, "wrong-token")
	_, err := v.Validate("cf_live_key")
	if err == nil {
		t.Fatal("expected error with wrong token")
	}
}

// TestFullWorkCycle tests: validate → start work → get briefing → complete work → check followups.
func TestFullWorkCycle(t *testing.T) {
	java := mockJavaServer(t, "tok", "tid-w1", "myteam")
	defer java.Close()

	redisClient, mr := newTestRedis(t)
	defer mr.Close()

	ctx := context.Background()

	validator := auth.NewValidator(java.URL, "tok")
	teamCtx, err := validator.Validate("cf_live_anykey")
	if err != nil {
		t.Fatalf("validate: %v", err)
	}

	sessionStore := session.NewStore(redisClient)
	workStore := work.NewStore(redisClient)

	// Create session
	sess := &session.Session{
		ID:          "sess-integration",
		TeamID:      teamCtx.Teams[0].TeamID,
		Agent:       "claude-code",
		ConnectedAt: time.Now().UTC(),
		LastSeenAt:  time.Now().UTC(),
	}
	if err := sessionStore.Create(ctx, sess, teamCtx.Teams[0].HeartbeatInterval); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// No active work initially
	active, _ := workStore.ListActive(ctx, teamCtx.Teams[0].TeamID)
	if len(active) != 0 {
		t.Fatalf("expected 0 active works, got %d", len(active))
	}

	// Start work
	w, err := workStore.Start(ctx, teamCtx.Teams[0].TeamID, "sess-int-1", "dev-mac", "adding JWT refresh rotation", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if w.Status != "active" {
		t.Errorf("expected active, got %s", w.Status)
	}

	// Another agent checks briefing — sees active work
	active, _ = workStore.ListActive(ctx, teamCtx.Teams[0].TeamID)
	if len(active) != 1 {
		t.Fatalf("expected 1 active work, got %d", len(active))
	}
	if active[0].Hostname != "dev-mac" {
		t.Errorf("wrong hostname: %s", active[0].Hostname)
	}

	// Complete work with followups
	followups := []work.Followup{
		{Description: "update auth middleware", Area: "src/middleware/", Priority: "critical", Type: "task"},
		{Description: "review the implementation before shipping", Priority: "normal", Type: "review"},
	}
	completed, err := workStore.Complete(ctx, teamCtx.Teams[0].TeamID, w.ID, "implemented refresh rotation, tokens now rotate on use", followups)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if completed.Status != "completed" {
		t.Errorf("expected completed, got %s", completed.Status)
	}

	// Active list is now empty
	active, _ = workStore.ListActive(ctx, teamCtx.Teams[0].TeamID)
	if len(active) != 0 {
		t.Fatalf("expected 0 active after complete, got %d", len(active))
	}

	// Recent list shows the completed work
	recent, _ := workStore.ListRecent(ctx, teamCtx.Teams[0].TeamID, 5)
	if len(recent) != 1 {
		t.Fatalf("expected 1 recent work, got %d", len(recent))
	}
	if recent[0].Result != "implemented refresh rotation, tokens now rotate on use" {
		t.Errorf("wrong result: %s", recent[0].Result)
	}

	// Followup IDs are set
	for _, f := range recent[0].Followups {
		if f.ID == "" {
			t.Errorf("followup %q missing ID", f.Description)
		}
	}

	// Followups are visible
	openFollowups, sources := workStore.AllOpenFollowups(ctx, teamCtx.Teams[0].TeamID)
	if len(openFollowups) != 2 {
		t.Fatalf("expected 2 followups, got %d", len(openFollowups))
	}
	if len(sources) != 2 {
		t.Fatalf("expected 2 sources, got %d", len(sources))
	}
	if openFollowups[0].Priority != "critical" {
		t.Errorf("expected critical priority first, got %s", openFollowups[0].Priority)
	}

	// Taking a followup removes it from open list
	takenID := recent[0].Followups[0].ID
	nextWork, _ := workStore.Start(ctx, teamCtx.Teams[0].TeamID, "sess-int-2", "dev-mac", "fixing auth middleware", nil)
	if err := workStore.TakeFollowup(ctx, teamCtx.Teams[0].TeamID, takenID, nextWork.ID); err != nil {
		t.Fatalf("TakeFollowup: %v", err)
	}
	open, _ := workStore.AllOpenFollowups(ctx, teamCtx.Teams[0].TeamID)
	if len(open) != 1 {
		t.Fatalf("expected 1 open followup after taking one, got %d", len(open))
	}

	// Cleanup session
	if err := sessionStore.Delete(ctx, teamCtx.Teams[0].TeamID, sess.ID); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	sessions, _ := sessionStore.List(ctx, teamCtx.Teams[0].TeamID)
	if len(sessions) != 0 {
		t.Fatalf("expected 0 sessions after delete, got %d", len(sessions))
	}
}
