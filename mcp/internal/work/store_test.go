package work

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestStore(t *testing.T) (*Store, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return NewStore(client), mr
}

func TestStore_StartAndListActive(t *testing.T) {
	store, mr := newTestStore(t)
	defer mr.Close()

	ctx := context.Background()
	teamID := "team-1"

	w, err := store.Start(ctx, teamID, "sess-1", "dev-mac", "adding refresh token rotation", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if w.ID == "" {
		t.Fatal("expected non-empty work ID")
	}
	if w.Status != "active" {
		t.Errorf("expected status=active, got %s", w.Status)
	}

	active, err := store.ListActive(ctx, teamID)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("expected 1 active work, got %d", len(active))
	}
	if active[0].Plan != "adding refresh token rotation" {
		t.Errorf("wrong plan: %s", active[0].Plan)
	}
}

func TestStore_Complete(t *testing.T) {
	store, mr := newTestStore(t)
	defer mr.Close()

	ctx := context.Background()
	teamID := "team-2"

	w, _ := store.Start(ctx, teamID, "sess-2", "host-2", "refactoring auth module", nil)

	followups := []Followup{
		{Description: "update middleware", Area: "src/middleware/", Priority: "critical", Type: "task"},
		{Description: "review the PR before merging", Priority: "normal", Type: "review"},
	}

	completed, err := store.Complete(ctx, teamID, w.ID, "refactored auth, moved to JWT", followups)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if completed.Status != "completed" {
		t.Errorf("expected status=completed, got %s", completed.Status)
	}
	if completed.Result != "refactored auth, moved to JWT" {
		t.Errorf("wrong result: %s", completed.Result)
	}
	if len(completed.Followups) != 2 {
		t.Fatalf("expected 2 followups, got %d", len(completed.Followups))
	}
	for _, f := range completed.Followups {
		if f.ID == "" {
			t.Errorf("followup %q missing ID", f.Description)
		}
	}
	if completed.CompletedAt == nil {
		t.Fatal("CompletedAt should be set")
	}

	active, _ := store.ListActive(ctx, teamID)
	if len(active) != 0 {
		t.Fatalf("expected 0 active works after completion, got %d", len(active))
	}
}

func TestStore_GetBySession(t *testing.T) {
	store, mr := newTestStore(t)
	defer mr.Close()

	ctx := context.Background()
	teamID := "team-3"

	_, _ = store.Start(ctx, teamID, "sess-A", "hostA", "work A", nil)
	_, _ = store.Start(ctx, teamID, "sess-B", "hostB", "work B", nil)

	w, err := store.GetBySession(ctx, teamID, "sess-A")
	if err != nil {
		t.Fatalf("GetBySession: %v", err)
	}
	if w == nil {
		t.Fatal("expected work for sess-A")
	}
	if w.Plan != "work A" {
		t.Errorf("wrong plan: %s", w.Plan)
	}

	missing, err := store.GetBySession(ctx, teamID, "sess-unknown")
	if err != nil {
		t.Fatalf("GetBySession for unknown: %v", err)
	}
	if missing != nil {
		t.Fatal("expected nil for unknown session")
	}
}

func TestStore_StartReplacesExistingWork(t *testing.T) {
	store, mr := newTestStore(t)
	defer mr.Close()

	ctx := context.Background()
	teamID := "team-4"

	_, _ = store.Start(ctx, teamID, "sess-1", "host", "first task", nil)
	w2, _ := store.Start(ctx, teamID, "sess-1", "host", "second task", nil)

	active, _ := store.ListActive(ctx, teamID)
	if len(active) != 1 {
		t.Fatalf("expected 1 active work (old replaced), got %d", len(active))
	}
	if active[0].ID != w2.ID {
		t.Errorf("active work should be the new one")
	}
}

func TestStore_RecentList(t *testing.T) {
	store, mr := newTestStore(t)
	defer mr.Close()

	ctx := context.Background()
	teamID := "team-5"

	for i := 0; i < 3; i++ {
		w, _ := store.Start(ctx, teamID, "sess-"+string(rune('A'+i)), "host", "task", nil)
		store.Complete(ctx, teamID, w.ID, "done", nil)
	}

	recent, err := store.ListRecent(ctx, teamID, 10)
	if err != nil {
		t.Fatalf("ListRecent: %v", err)
	}
	if len(recent) != 3 {
		t.Fatalf("expected 3 recent works, got %d", len(recent))
	}
}

func TestStore_IsolatedByTeam(t *testing.T) {
	store, mr := newTestStore(t)
	defer mr.Close()

	ctx := context.Background()

	store.Start(ctx, "team-A", "sess-1", "host", "work for A", nil)
	store.Start(ctx, "team-B", "sess-2", "host", "work for B", nil)

	aActive, _ := store.ListActive(ctx, "team-A")
	bActive, _ := store.ListActive(ctx, "team-B")

	if len(aActive) != 1 || len(bActive) != 1 {
		t.Fatalf("each team should have 1 active work: A=%d B=%d", len(aActive), len(bActive))
	}
	if aActive[0].Plan == bActive[0].Plan {
		t.Fatal("team isolation broken")
	}
}

func TestStore_AllOpenFollowups(t *testing.T) {
	store, mr := newTestStore(t)
	defer mr.Close()

	ctx := context.Background()
	teamID := "team-6"

	w1, _ := store.Start(ctx, teamID, "sess-1", "host", "task one", nil)
	store.Complete(ctx, teamID, w1.ID, "done", []Followup{
		{Description: "followup A", Priority: "critical"},
		{Description: "followup B", Priority: "normal"},
	})

	w2, _ := store.Start(ctx, teamID, "sess-2", "host", "task two", nil)
	store.Complete(ctx, teamID, w2.ID, "done", []Followup{
		{Description: "followup C"},
	})

	followups, sources := store.AllOpenFollowups(ctx, teamID)
	if len(followups) != 3 {
		t.Fatalf("expected 3 followups, got %d", len(followups))
	}
	if len(sources) != 3 {
		t.Fatalf("expected 3 sources, got %d", len(sources))
	}
}

func TestStore_TakeFollowup(t *testing.T) {
	store, mr := newTestStore(t)
	defer mr.Close()

	ctx := context.Background()
	teamID := "team-take"

	w1, _ := store.Start(ctx, teamID, "sess-1", "host", "task one", nil)
	completed, _ := store.Complete(ctx, teamID, w1.ID, "done", []Followup{
		{Description: "review the changes", Priority: "normal", Type: "review"},
		{Description: "update docs", Priority: "low", Type: "docs"},
	})

	followups, _ := store.AllOpenFollowups(ctx, teamID)
	if len(followups) != 2 {
		t.Fatalf("expected 2 open followups, got %d", len(followups))
	}

	reviewID := completed.Followups[0].ID

	w2, _ := store.Start(ctx, teamID, "sess-2", "host", "reviewing the changes", nil)
	if err := store.TakeFollowup(ctx, teamID, reviewID, w2.ID); err != nil {
		t.Fatalf("TakeFollowup: %v", err)
	}

	open, _ := store.AllOpenFollowups(ctx, teamID)
	if len(open) != 1 {
		t.Fatalf("expected 1 open followup after taking one, got %d", len(open))
	}
	if open[0].Description != "update docs" {
		t.Errorf("wrong remaining followup: %s", open[0].Description)
	}
}

// TestStore_TakeFollowupDoublePickIsRejected locks in the atomicity of
// claiming a followup. The previous implementation used HSET, which silently
// overwrote an earlier claim — so two agents picking the same followup_id
// both "succeeded" and later coordination broke. The fix is HSETNX.
func TestStore_TakeFollowupDoublePickIsRejected(t *testing.T) {
	store, mr := newTestStore(t)
	defer mr.Close()

	ctx := context.Background()
	teamID := "team-race"

	w1, _ := store.Start(ctx, teamID, "sess-1", "host", "parent task", nil)
	completed, _ := store.Complete(ctx, teamID, w1.ID, "done", []Followup{
		{Description: "shared followup", Priority: "normal"},
	})
	followupID := completed.Followups[0].ID

	firstClaim, _ := store.Start(ctx, teamID, "sess-A", "host-A", "taking the followup", nil)
	if err := store.TakeFollowup(ctx, teamID, followupID, firstClaim.ID); err != nil {
		t.Fatalf("first TakeFollowup should succeed: %v", err)
	}

	secondClaim, _ := store.Start(ctx, teamID, "sess-B", "host-B", "also taking the followup", nil)
	err := store.TakeFollowup(ctx, teamID, followupID, secondClaim.ID)
	if !errors.Is(err, ErrFollowupAlreadyTaken) {
		t.Fatalf("second TakeFollowup should return ErrFollowupAlreadyTaken, got %v", err)
	}

	taker, err := store.FollowupTaker(ctx, teamID, followupID)
	if err != nil {
		t.Fatalf("FollowupTaker: %v", err)
	}
	if taker != firstClaim.ID {
		t.Errorf("taker = %s, want %s (first claim should win)", taker, firstClaim.ID)
	}
}

func TestStore_RecordConflict(t *testing.T) {
	store, mr := newTestStore(t)
	defer mr.Close()

	ctx := context.Background()
	teamID := "team-conflicts"

	for i := 0; i < 3; i++ {
		err := store.RecordConflict(ctx, teamID, []ConflictEvent{{
			ID:                  fmt.Sprintf("conflict-%d", i),
			Path:                "src/shared.ts",
			RequestedBySession:  "sess-new",
			RequestedByHostname: "host-a",
			HeldBySession:       "sess-old",
			HeldByHostname:      "host-b",
			Mode:                "warn",
			DetectedAt:          "2026-04-13T00:00:00Z",
		}})
		if err != nil {
			t.Fatalf("RecordConflict: %v", err)
		}
	}

	// List should have 3 entries and respect cap-at-100 semantics.
	key := store.conflictsKey(teamID)
	count, err := store.redis.LLen(ctx, key).Result()
	if err != nil {
		t.Fatalf("LLen: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 conflict entries, got %d", count)
	}
}

func TestStore_TTLExpiry(t *testing.T) {
	store, mr := newTestStore(t)
	defer mr.Close()

	ctx := context.Background()
	teamID := "team-7"

	_, _ = store.Start(ctx, teamID, "sess-ttl", "host", "will expire", nil)

	mr.FastForward(25 * time.Hour)

	active, _ := store.ListActive(ctx, teamID)
	if len(active) != 0 {
		t.Fatalf("expected 0 active works after TTL, got %d", len(active))
	}
}
