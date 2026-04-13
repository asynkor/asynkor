package work

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

type Followup struct {
	ID            string `json:"id"`
	Description   string `json:"description"`
	Area          string `json:"area,omitempty"`
	Priority      string `json:"priority,omitempty"`
	Type          string `json:"type,omitempty"`
	Context       string `json:"context,omitempty"`
	Tried         string `json:"tried,omitempty"`
	WatchOut      string `json:"watch_out,omitempty"`
	TakenByWorkID string `json:"taken_by_work_id,omitempty"`
}

type Work struct {
	ID        string `json:"id"`
	TeamID    string `json:"team_id"`
	SessionID string `json:"session_id"`
	Hostname  string `json:"hostname"`
	Plan      string `json:"plan"`
	Result    string `json:"result,omitempty"`
	Learnings string `json:"learnings,omitempty"`
	Decisions string `json:"decisions,omitempty"`
	// PlannedPaths is the set of files the agent *declared* it intends to
	// touch when calling asynkor_start. Populated at start time so overlap
	// detection can run while work is still in progress — unlike
	// FilesTouched which is only set at asynkor_finish.
	PlannedPaths []string   `json:"planned_paths,omitempty"`
	FilesTouched []string   `json:"files_touched,omitempty"`
	Followups    []Followup `json:"followups,omitempty"`
	Progress     string     `json:"progress,omitempty"`
	ParkedAt     *time.Time `json:"parked_at,omitempty"`
	ParkedNotes  string     `json:"parked_notes,omitempty"`
	HandoffFrom  string     `json:"handoff_from,omitempty"`
	Status       string     `json:"status"`
	CreatedAt    time.Time  `json:"created_at"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
}

type Store struct {
	redis *redis.Client
}

func NewStore(r *redis.Client) *Store {
	return &Store{redis: r}
}

// startScript atomically removes the old work for a session, sets the new
// work data, adds it to the active set, and maps session to work ID.
// KEYS[1] = work key, KEYS[2] = active set, KEYS[3] = session→work mapping
// ARGV[1] = work data, ARGV[2] = TTL seconds, ARGV[3] = work ID
var startScript = redis.NewScript(`
	local oldWorkID = redis.call('GET', KEYS[3])
	if oldWorkID then
		redis.call('SREM', KEYS[2], oldWorkID)
	end
	redis.call('SET', KEYS[1], ARGV[1], 'EX', ARGV[2])
	redis.call('SADD', KEYS[2], ARGV[3])
	redis.call('SET', KEYS[3], ARGV[3], 'EX', ARGV[2])
	return 1
`)

// completeScript atomically updates a work item to completed status,
// removes it from the active set, deletes the session mapping, and
// pushes it to the recent list.
// KEYS[1] = work key, KEYS[2] = active set, KEYS[3] = session→work, KEYS[4] = recent list
// ARGV[1] = updated work data, ARGV[2] = TTL seconds, ARGV[3] = work ID
var completeScript = redis.NewScript(`
	local data = redis.call('GET', KEYS[1])
	if not data then
		return redis.error_reply('work not found')
	end
	redis.call('SET', KEYS[1], ARGV[1], 'EX', ARGV[2])
	redis.call('SREM', KEYS[2], ARGV[3])
	redis.call('DEL', KEYS[3])
	redis.call('LPUSH', KEYS[4], ARGV[1])
	redis.call('LTRIM', KEYS[4], 0, 29)
	return 1
`)

var parkScript = redis.NewScript(`
	local data = redis.call('GET', KEYS[1])
	if not data then
		return redis.error_reply('work not found')
	end
	redis.call('SET', KEYS[1], ARGV[1], 'EX', ARGV[2])
	redis.call('SREM', KEYS[2], ARGV[3])
	redis.call('DEL', KEYS[3])
	redis.call('LPUSH', KEYS[4], ARGV[1])
	redis.call('LTRIM', KEYS[4], 0, 29)
	return 1
`)

func (s *Store) workKey(teamID, workID string) string {
	return fmt.Sprintf("team:%s:work:%s", teamID, workID)
}

func (s *Store) activeSetKey(teamID string) string {
	return fmt.Sprintf("team:%s:works:active", teamID)
}

func (s *Store) recentListKey(teamID string) string {
	return fmt.Sprintf("team:%s:works:recent", teamID)
}

func (s *Store) parkedListKey(teamID string) string {
	return fmt.Sprintf("team:%s:works:parked", teamID)
}

func (s *Store) sessionWorkKey(teamID, sessionID string) string {
	return fmt.Sprintf("team:%s:session:%s:work_id", teamID, sessionID)
}

func (s *Store) takenKey(teamID string) string {
	return fmt.Sprintf("team:%s:followups:taken", teamID)
}

func (s *Store) conflictsKey(teamID string) string {
	return fmt.Sprintf("team:%s:conflicts", teamID)
}

// ConflictEvent matches the shape the Java backend and frontend expect.
// One event per overlapping path/plan so the dashboard can show each conflict
// with both the requester and holder hostnames.
type ConflictEvent struct {
	ID                     string `json:"id"`
	Path                   string `json:"path"`
	RequestedBySession     string `json:"requested_by_session"`
	RequestedByAgent       string `json:"requested_by_agent"`
	RequestedByHostname    string `json:"requested_by_hostname"`
	HeldBySession          string `json:"held_by_session"`
	HeldByHostname         string `json:"held_by_hostname"`
	Mode                   string `json:"mode"`
	DetectedAt             string `json:"detected_at"`
}

// RecordConflict appends one or more conflict events to the team's feed,
// capped at 100 entries.
func (s *Store) RecordConflict(ctx context.Context, teamID string, events []ConflictEvent) error {
	if len(events) == 0 {
		return nil
	}
	pipe := s.redis.TxPipeline()
	for _, ev := range events {
		data, err := json.Marshal(ev)
		if err != nil {
			continue
		}
		pipe.LPush(ctx, s.conflictsKey(teamID), data)
	}
	pipe.LTrim(ctx, s.conflictsKey(teamID), 0, 99)
	_, err := pipe.Exec(ctx)
	return err
}

func (s *Store) Start(ctx context.Context, teamID, sessionID, hostname, plan string, plannedPaths []string) (*Work, error) {
	w := &Work{
		ID:           uuid.New().String(),
		TeamID:       teamID,
		SessionID:    sessionID,
		Hostname:     hostname,
		Plan:         plan,
		PlannedPaths: plannedPaths,
		Status:       "active",
		CreatedAt:    time.Now().UTC(),
	}
	data, err := json.Marshal(w)
	if err != nil {
		return nil, fmt.Errorf("marshal work: %w", err)
	}

	ttlSeconds := int(24 * time.Hour / time.Second)
	keys := []string{
		s.workKey(teamID, w.ID),
		s.activeSetKey(teamID),
		s.sessionWorkKey(teamID, sessionID),
	}
	err = startScript.Run(ctx, s.redis, keys, string(data), ttlSeconds, w.ID).Err()
	return w, err
}

// ErrFollowupAlreadyTaken is returned by TakeFollowup when another work has
// already claimed the same followup ID. The spec's promise of "double-picking
// prevented" depends on this being atomic — a previous implementation used
// HSET, which silently overwrote the prior claim.
var ErrFollowupAlreadyTaken = errors.New("followup already taken")

func (s *Store) TakeFollowup(ctx context.Context, teamID, followupID, workID string) error {
	ok, err := s.redis.HSetNX(ctx, s.takenKey(teamID), followupID, workID).Result()
	if err != nil {
		return err
	}
	if !ok {
		return ErrFollowupAlreadyTaken
	}
	return nil
}

// FollowupTaker returns the work ID that currently holds the given followup,
// or empty string if it is not taken. Used to tell an agent who already has
// the followup so it can coordinate instead of retrying.
func (s *Store) FollowupTaker(ctx context.Context, teamID, followupID string) (string, error) {
	v, err := s.redis.HGet(ctx, s.takenKey(teamID), followupID).Result()
	if err == redis.Nil {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return v, nil
}

func (s *Store) Complete(ctx context.Context, teamID, workID, result string, followups []Followup) (*Work, error) {
	data, err := s.redis.Get(ctx, s.workKey(teamID, workID)).Bytes()
	if err != nil {
		return nil, err
	}
	var w Work
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	w.Status = "completed"
	w.Result = result
	w.Followups = followups
	if w.Followups == nil {
		w.Followups = []Followup{}
	}
	for i := range w.Followups {
		if w.Followups[i].ID == "" {
			w.Followups[i].ID = uuid.New().String()
		}
	}
	w.CompletedAt = &now

	updated, err := json.Marshal(w)
	if err != nil {
		return nil, fmt.Errorf("marshal work: %w", err)
	}

	ttlSeconds := int(72 * time.Hour / time.Second)
	keys := []string{
		s.workKey(teamID, workID),
		s.activeSetKey(teamID),
		s.sessionWorkKey(teamID, w.SessionID),
		s.recentListKey(teamID),
	}
	err = completeScript.Run(ctx, s.redis, keys, string(updated), ttlSeconds, workID).Err()
	return &w, err
}

func (s *Store) GetBySession(ctx context.Context, teamID, sessionID string) (*Work, error) {
	workID, err := s.redis.Get(ctx, s.sessionWorkKey(teamID, sessionID)).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get session work: %w", err)
	}
	data, err := s.redis.Get(ctx, s.workKey(teamID, workID)).Bytes()
	if err != nil {
		return nil, nil
	}
	var w Work
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, err
	}
	return &w, nil
}

func (s *Store) ListActive(ctx context.Context, teamID string) ([]*Work, error) {
	ids, err := s.redis.SMembers(ctx, s.activeSetKey(teamID)).Result()
	if err != nil {
		return nil, err
	}
	var works []*Work
	for _, id := range ids {
		data, err := s.redis.Get(ctx, s.workKey(teamID, id)).Bytes()
		if err != nil {
			s.redis.SRem(ctx, s.activeSetKey(teamID), id)
			continue
		}
		var w Work
		if json.Unmarshal(data, &w) == nil {
			works = append(works, &w)
		}
	}
	return works, nil
}

func (s *Store) ListRecent(ctx context.Context, teamID string, limit int) ([]*Work, error) {
	if limit <= 0 {
		limit = 10
	}
	vals, err := s.redis.LRange(ctx, s.recentListKey(teamID), 0, int64(limit-1)).Result()
	if err != nil {
		return nil, err
	}
	var works []*Work
	for _, v := range vals {
		var w Work
		if json.Unmarshal([]byte(v), &w) == nil {
			works = append(works, &w)
		}
	}
	return works, nil
}

func (s *Store) Park(ctx context.Context, teamID, workID, progress, notes string) (*Work, error) {
	data, err := s.redis.Get(ctx, s.workKey(teamID, workID)).Bytes()
	if err != nil {
		return nil, err
	}
	var w Work
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	w.Status = "parked"
	w.Progress = progress
	w.ParkedNotes = notes
	w.ParkedAt = &now

	updated, err := json.Marshal(w)
	if err != nil {
		return nil, fmt.Errorf("marshal work: %w", err)
	}

	ttlSeconds := int(168 * time.Hour / time.Second)
	keys := []string{
		s.workKey(teamID, workID),
		s.activeSetKey(teamID),
		s.sessionWorkKey(teamID, w.SessionID),
		s.parkedListKey(teamID),
	}
	err = parkScript.Run(ctx, s.redis, keys, string(updated), ttlSeconds, workID).Err()
	return &w, err
}

func (s *Store) ListParked(ctx context.Context, teamID string) ([]*Work, error) {
	vals, err := s.redis.LRange(ctx, s.parkedListKey(teamID), 0, 29).Result()
	if err != nil {
		return nil, err
	}
	var works []*Work
	for _, v := range vals {
		var w Work
		if json.Unmarshal([]byte(v), &w) == nil {
			works = append(works, &w)
		}
	}
	return works, nil
}

func (s *Store) ResumeParked(ctx context.Context, teamID, workID, newSessionID, newHostname string) (*Work, error) {
	data, err := s.redis.Get(ctx, s.workKey(teamID, workID)).Bytes()
	if err != nil {
		return nil, err
	}
	var parked Work
	if err := json.Unmarshal(data, &parked); err != nil {
		return nil, err
	}

	s.redis.LRem(ctx, s.parkedListKey(teamID), 1, string(data))

	w := &Work{
		ID:           uuid.New().String(),
		TeamID:       teamID,
		SessionID:    newSessionID,
		Hostname:     newHostname,
		Plan:         parked.Plan,
		PlannedPaths: parked.PlannedPaths,
		HandoffFrom:  workID,
		Status:       "active",
		CreatedAt:    time.Now().UTC(),
	}
	newData, err := json.Marshal(w)
	if err != nil {
		return nil, fmt.Errorf("marshal work: %w", err)
	}

	ttlSeconds := int(24 * time.Hour / time.Second)
	keys := []string{
		s.workKey(teamID, w.ID),
		s.activeSetKey(teamID),
		s.sessionWorkKey(teamID, newSessionID),
	}
	err = startScript.Run(ctx, s.redis, keys, string(newData), ttlSeconds, w.ID).Err()
	return w, err
}

func (s *Store) GetByID(ctx context.Context, teamID, workID string) (*Work, error) {
	data, err := s.redis.Get(ctx, s.workKey(teamID, workID)).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var w Work
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, err
	}
	return &w, nil
}

func (s *Store) CleanupStale(ctx context.Context, teamID string, maxAge time.Duration) (int, error) {
	ids, err := s.redis.SMembers(ctx, s.activeSetKey(teamID)).Result()
	if err != nil {
		return 0, err
	}
	cleaned := 0
	for _, id := range ids {
		data, err := s.redis.Get(ctx, s.workKey(teamID, id)).Bytes()
		if err == redis.Nil {
			s.redis.SRem(ctx, s.activeSetKey(teamID), id)
			cleaned++
			continue
		}
		if err != nil {
			continue
		}
		var w Work
		if err := json.Unmarshal(data, &w); err != nil {
			continue
		}
		if time.Since(w.CreatedAt) > maxAge {
			s.redis.SRem(ctx, s.activeSetKey(teamID), id)
			cleaned++
		}
	}
	return cleaned, nil
}

// Cancel removes a work item from the active set and deletes its data.
// Used for manual cleanup of stale/orphaned work items.
func (s *Store) Cancel(ctx context.Context, teamID, workID string) error {
	data, err := s.redis.Get(ctx, s.workKey(teamID, workID)).Bytes()
	if err == redis.Nil {
		// Work data already gone — just clean up the active set reference.
		s.redis.SRem(ctx, s.activeSetKey(teamID), workID)
		return nil
	}
	if err != nil {
		return err
	}
	var w Work
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	s.redis.SRem(ctx, s.activeSetKey(teamID), workID)
	s.redis.Del(ctx, s.workKey(teamID, workID))
	s.redis.Del(ctx, s.sessionWorkKey(teamID, w.SessionID))
	return nil
}

func (s *Store) AllOpenFollowups(ctx context.Context, teamID string) ([]Followup, []string) {
	taken, _ := s.redis.HGetAll(ctx, s.takenKey(teamID)).Result()

	recent, err := s.ListRecent(ctx, teamID, 20)
	if err != nil {
		return nil, nil
	}
	var followups []Followup
	var sources []string
	for _, w := range recent {
		for _, f := range w.Followups {
			if _, isTaken := taken[f.ID]; isTaken {
				continue
			}
			followups = append(followups, f)
			src := w.Plan
			if len(src) > 60 {
				src = src[:60] + "…"
			}
			sources = append(sources, src)
		}
	}
	return followups, sources
}
