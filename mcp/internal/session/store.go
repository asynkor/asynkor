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

// Active team override for user-scoped keys. Keyed globally (not per team)
// because the whole point is letting a session switch between teams without
// recreating its team-bound record.
//
// On every Server.makeContextFunc call we look up this key first; if set
// and still a valid member of the user's accessible teams, we bind the
// session to that team. Otherwise we fall back to the key's default team.
func (s *Store) activeTeamKey(sessionID string) string {
	return fmt.Sprintf("session:%s:active_team_id", sessionID)
}

func (s *Store) GetActiveTeam(ctx context.Context, sessionID string) (string, error) {
	v, err := s.redis.Get(ctx, s.activeTeamKey(sessionID)).Result()
	if err == redis.Nil {
		return "", nil
	}
	return v, err
}

func (s *Store) SetActiveTeam(ctx context.Context, sessionID, teamID string, heartbeatInterval int) error {
	ttl := time.Duration(heartbeatInterval*3) * time.Second
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return s.redis.Set(ctx, s.activeTeamKey(sessionID), teamID, ttl).Err()
}

func (s *Store) ClearActiveTeam(ctx context.Context, sessionID string) error {
	return s.redis.Del(ctx, s.activeTeamKey(sessionID)).Err()
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
