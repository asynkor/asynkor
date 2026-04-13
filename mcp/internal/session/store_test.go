package session

import (
	"context"
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

func TestStore_CreateAndList(t *testing.T) {
	store, mr := newTestStore(t)
	defer mr.Close()

	ctx := context.Background()
	teamID := "team-1"

	sess := &Session{
		ID:           "sess-abc",
		TeamID:       teamID,
		Agent:        "claude-code",
		AgentVersion: "1.0.0",
		Hostname:     "dev-macbook",
		ConnectedAt:  time.Now().UTC(),
		LastSeenAt:   time.Now().UTC(),
	}

	if err := store.Create(ctx, sess, 60); err != nil {
		t.Fatalf("Create: %v", err)
	}

	sessions, err := store.List(ctx, teamID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].ID != sess.ID {
		t.Errorf("wrong session ID: %s", sessions[0].ID)
	}
	if sessions[0].Agent != "claude-code" {
		t.Errorf("wrong agent: %s", sessions[0].Agent)
	}
	if sessions[0].Hostname != "dev-macbook" {
		t.Errorf("wrong hostname: %s", sessions[0].Hostname)
	}
}

func TestStore_Delete(t *testing.T) {
	store, mr := newTestStore(t)
	defer mr.Close()

	ctx := context.Background()
	teamID := "team-2"

	sess := &Session{ID: "sess-del", TeamID: teamID, ConnectedAt: time.Now(), LastSeenAt: time.Now()}
	store.Create(ctx, sess, 60)

	if err := store.Delete(ctx, teamID, sess.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	sessions, _ := store.List(ctx, teamID)
	if len(sessions) != 0 {
		t.Fatalf("expected 0 sessions after delete, got %d", len(sessions))
	}
}

func TestStore_ListEmpty(t *testing.T) {
	store, mr := newTestStore(t)
	defer mr.Close()

	sessions, err := store.List(context.Background(), "nonexistent-team")
	if err != nil {
		t.Fatalf("List on empty team: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("expected 0 sessions, got %d", len(sessions))
	}
}

func TestStore_SessionTTL(t *testing.T) {
	store, mr := newTestStore(t)
	defer mr.Close()

	ctx := context.Background()
	teamID := "team-3"

	sess := &Session{ID: "sess-ttl", TeamID: teamID, ConnectedAt: time.Now(), LastSeenAt: time.Now()}
	// heartbeatInterval=2, so TTL = 6s
	store.Create(ctx, sess, 2)

	// FastForward past TTL
	mr.FastForward(7 * time.Second)

	sessions, _ := store.List(ctx, teamID)
	for _, s := range sessions {
		if s.ID == sess.ID {
			t.Fatal("expired session should not appear in list")
		}
	}
}

func TestStore_MultipleSessionsSameTeam(t *testing.T) {
	store, mr := newTestStore(t)
	defer mr.Close()

	ctx := context.Background()
	teamID := "team-4"

	for i, id := range []string{"sess-1", "sess-2", "sess-3"} {
		sess := &Session{
			ID:     id,
			TeamID: teamID,
			Agent:  "claude-code",
			Hostname: "machine-" + string(rune('A'+i)),
			ConnectedAt: time.Now(),
			LastSeenAt:  time.Now(),
		}
		store.Create(ctx, sess, 60)
	}

	sessions, _ := store.List(ctx, teamID)
	if len(sessions) != 3 {
		t.Fatalf("expected 3 sessions, got %d", len(sessions))
	}
}

func TestStore_IsolatedByTeam(t *testing.T) {
	store, mr := newTestStore(t)
	defer mr.Close()

	ctx := context.Background()

	store.Create(ctx, &Session{ID: "s1", TeamID: "team-A", ConnectedAt: time.Now(), LastSeenAt: time.Now()}, 60)
	store.Create(ctx, &Session{ID: "s2", TeamID: "team-B", ConnectedAt: time.Now(), LastSeenAt: time.Now()}, 60)

	aList, _ := store.List(ctx, "team-A")
	bList, _ := store.List(ctx, "team-B")

	if len(aList) != 1 {
		t.Fatalf("team-A should have 1 session, got %d", len(aList))
	}
	if len(bList) != 1 {
		t.Fatalf("team-B should have 1 session, got %d", len(bList))
	}
	if aList[0].ID == bList[0].ID {
		t.Fatal("teams should have different sessions")
	}
}
