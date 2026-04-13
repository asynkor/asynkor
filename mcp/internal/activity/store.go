package activity

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

const maxActivity = 200

type Event struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
	Agent     string `json:"agent"`
	Hostname  string `json:"hostname"`
	Path      string `json:"path,omitempty"`
	Timestamp string `json:"timestamp"`
}

type Store struct {
	redis *redis.Client
}

func NewStore(r *redis.Client) *Store {
	return &Store{redis: r}
}

func (s *Store) activityKey(teamID string) string {
	return fmt.Sprintf("team:%s:activity", teamID)
}

func (s *Store) RecordEvent(ctx context.Context, teamID string, event Event) {
	if event.Timestamp == "" {
		event.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	data, err := json.Marshal(event)
	if err != nil {
		return
	}
	key := s.activityKey(teamID)
	pipe := s.redis.Pipeline()
	pipe.LPush(ctx, key, string(data))
	pipe.LTrim(ctx, key, 0, maxActivity-1)
	if _, err := pipe.Exec(ctx); err != nil {
		log.Printf("activity: record event failed: %v", err)
	}
}
