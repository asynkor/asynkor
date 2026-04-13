package session

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

type Session struct {
	ID           string    `json:"id"`
	TeamID       string    `json:"team_id"`
	Agent        string    `json:"agent"`
	AgentVersion string    `json:"agent_version"`
	Hostname     string    `json:"hostname"`
	ConnectedAt  time.Time `json:"connected_at"`
	LastSeenAt   time.Time `json:"last_seen_at"`
}

type Store struct {
	redis *redis.Client
}

func NewStore(r *redis.Client) *Store {
	return &Store{redis: r}
}

func (s *Store) sessionKey(teamID, sessionID string) string {
	return fmt.Sprintf("team:%s:session:%s", teamID, sessionID)
}

func (s *Store) sessionsSetKey(teamID string) string {
	return fmt.Sprintf("team:%s:sessions", teamID)
}

func (s *Store) Create(ctx context.Context, sess *Session, heartbeatInterval int) error {
	data, err := json.Marshal(sess)
	if err != nil {
		return err
	}
	ttl := time.Duration(heartbeatInterval*3) * time.Second

	pipe := s.redis.TxPipeline()
	pipe.Set(ctx, s.sessionKey(sess.TeamID, sess.ID), data, ttl)
	pipe.SAdd(ctx, s.sessionsSetKey(sess.TeamID), sess.ID)
	_, err = pipe.Exec(ctx)
	return err
}

func (s *Store) Delete(ctx context.Context, teamID, sessionID string) error {
	pipe := s.redis.TxPipeline()
	pipe.Del(ctx, s.sessionKey(teamID, sessionID))
	pipe.SRem(ctx, s.sessionsSetKey(teamID), sessionID)
	_, err := pipe.Exec(ctx)
	return err
}

func (s *Store) List(ctx context.Context, teamID string) ([]*Session, error) {
	ids, err := s.redis.SMembers(ctx, s.sessionsSetKey(teamID)).Result()
	if err != nil {
		return nil, err
	}

	var sessions []*Session
	for _, id := range ids {
		data, err := s.redis.Get(ctx, s.sessionKey(teamID, id)).Bytes()
		if err != nil {
			s.redis.SRem(ctx, s.sessionsSetKey(teamID), id)
			continue
		}
		var sess Session
		if err := json.Unmarshal(data, &sess); err != nil {
			continue
		}
		sessions = append(sessions, &sess)
	}
	return sessions, nil
}
