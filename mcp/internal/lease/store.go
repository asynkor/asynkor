package lease

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const DefaultTTL = 5 * time.Minute

type Lease struct {
	Path       string    `json:"path"`
	WorkID     string    `json:"work_id"`
	SessionID  string    `json:"session_id"`
	Hostname   string    `json:"hostname"`
	Plan       string    `json:"plan"`
	AcquiredAt time.Time `json:"acquired_at"`
}

type BlockedLease struct {
	Path   string
	Holder *Lease
}

type Store struct {
	redis *redis.Client
}

func NewStore(r *redis.Client) *Store {
	return &Store{redis: r}
}

func (s *Store) leaseKey(teamID, path string) string {
	return fmt.Sprintf("team:%s:lease:%s", teamID, path)
}

func (s *Store) activeSetKey(teamID string) string {
	return fmt.Sprintf("team:%s:leases:active", teamID)
}

func (s *Store) workLeasesKey(teamID, workID string) string {
	return fmt.Sprintf("team:%s:work:%s:leases", teamID, workID)
}

// acquireScript atomically checks/acquires a lease.
// KEYS[1] = lease key, KEYS[2] = active set, KEYS[3] = work lease set
// ARGV[1] = lease JSON, ARGV[2] = TTL seconds, ARGV[3] = work_id, ARGV[4] = path
var acquireScript = redis.NewScript(`
	local existing = redis.call('GET', KEYS[1])
	if existing then
		local data = cjson.decode(existing)
		if data.work_id ~= ARGV[3] then
			return existing
		end
	end
	redis.call('SET', KEYS[1], ARGV[1], 'EX', ARGV[2])
	redis.call('SADD', KEYS[2], KEYS[1])
	redis.call('SADD', KEYS[3], ARGV[4])
	return nil
`)

func (s *Store) Acquire(ctx context.Context, teamID, path, workID, sessionID, hostname, plan string) (bool, *Lease, error) {
	l := &Lease{
		Path:       path,
		WorkID:     workID,
		SessionID:  sessionID,
		Hostname:   hostname,
		Plan:       plan,
		AcquiredAt: time.Now().UTC(),
	}
	data, err := json.Marshal(l)
	if err != nil {
		return false, nil, fmt.Errorf("marshal lease: %w", err)
	}

	ttlSeconds := int(DefaultTTL / time.Second)
	keys := []string{
		s.leaseKey(teamID, path),
		s.activeSetKey(teamID),
		s.workLeasesKey(teamID, workID),
	}
	result, err := acquireScript.Run(ctx, s.redis, keys, string(data), ttlSeconds, workID, path).Result()
	if err == redis.Nil {
		return true, nil, nil
	}
	if err != nil {
		return false, nil, err
	}

	var holder Lease
	if err := json.Unmarshal([]byte(result.(string)), &holder); err != nil {
		return false, nil, fmt.Errorf("unmarshal holder: %w", err)
	}
	return false, &holder, nil
}

func (s *Store) AcquireMany(ctx context.Context, teamID string, paths []string, workID, sessionID, hostname, plan string) ([]string, []BlockedLease, error) {
	var acquired []string
	var blocked []BlockedLease
	for _, p := range paths {
		ok, holder, err := s.Acquire(ctx, teamID, p, workID, sessionID, hostname, plan)
		if err != nil {
			return acquired, blocked, err
		}
		if ok {
			acquired = append(acquired, p)
		} else {
			blocked = append(blocked, BlockedLease{Path: p, Holder: holder})
		}
	}
	return acquired, blocked, nil
}

// releaseScript atomically checks work_id ownership then deletes the lease.
// KEYS[1] = lease key, KEYS[2] = active set
// ARGV[1] = work_id
var releaseScript = redis.NewScript(`
	local existing = redis.call('GET', KEYS[1])
	if not existing then return 1 end
	local data = cjson.decode(existing)
	if data.work_id == ARGV[1] then
		redis.call('DEL', KEYS[1])
		redis.call('SREM', KEYS[2], KEYS[1])
	end
	return 1
`)

func (s *Store) ReleaseByWork(ctx context.Context, teamID, workID string) error {
	wKey := s.workLeasesKey(teamID, workID)
	paths, err := s.redis.SMembers(ctx, wKey).Result()
	if err != nil {
		return err
	}
	for _, p := range paths {
		keys := []string{s.leaseKey(teamID, p), s.activeSetKey(teamID)}
		if err := releaseScript.Run(ctx, s.redis, keys, workID).Err(); err != nil {
			return err
		}
	}
	s.redis.Del(ctx, wKey)
	return nil
}

func (s *Store) CheckPaths(ctx context.Context, teamID string, paths []string) (map[string]*Lease, error) {
	result := make(map[string]*Lease)
	for _, p := range paths {
		data, err := s.redis.Get(ctx, s.leaseKey(teamID, p)).Bytes()
		if err == redis.Nil {
			continue
		}
		if err != nil {
			return nil, err
		}
		var l Lease
		if err := json.Unmarshal(data, &l); err != nil {
			return nil, err
		}
		result[p] = &l
	}
	return result, nil
}

func (s *Store) ListAll(ctx context.Context, teamID string) ([]*Lease, error) {
	keys, err := s.redis.SMembers(ctx, s.activeSetKey(teamID)).Result()
	if err != nil {
		return nil, err
	}
	var leases []*Lease
	for _, k := range keys {
		data, err := s.redis.Get(ctx, k).Bytes()
		if err != nil {
			s.redis.SRem(ctx, s.activeSetKey(teamID), k)
			continue
		}
		var l Lease
		if json.Unmarshal(data, &l) == nil {
			leases = append(leases, &l)
		}
	}
	return leases, nil
}

func (s *Store) WaitAndAcquire(ctx context.Context, teamID string, paths []string, workID, sessionID, hostname, plan string, timeout time.Duration) ([]string, []BlockedLease, error) {
	deadline := time.Now().Add(timeout)

	for {
		acquired, blocked, err := s.AcquireMany(ctx, teamID, paths, workID, sessionID, hostname, plan)
		if err != nil {
			return nil, nil, err
		}
		if len(blocked) == 0 {
			return acquired, nil, nil
		}

		// Release partial acquires for all-or-nothing semantics.
		for _, p := range acquired {
			keys := []string{s.leaseKey(teamID, p), s.activeSetKey(teamID)}
			_ = releaseScript.Run(ctx, s.redis, keys, workID).Err()
		}
		// Also clean up the work lease set for released paths.
		if len(acquired) > 0 {
			members := make([]interface{}, len(acquired))
			for i, p := range acquired {
				members[i] = p
			}
			s.redis.SRem(ctx, s.workLeasesKey(teamID, workID), members...)
		}

		if timeout == 0 || time.Now().After(deadline) {
			return nil, blocked, nil
		}

		select {
		case <-ctx.Done():
			return nil, blocked, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (s *Store) RefreshByWork(ctx context.Context, teamID, workID string) error {
	paths, err := s.redis.SMembers(ctx, s.workLeasesKey(teamID, workID)).Result()
	if err != nil {
		return err
	}
	ttl := DefaultTTL
	for _, p := range paths {
		if err := s.redis.Expire(ctx, s.leaseKey(teamID, p), ttl).Err(); err != nil {
			return err
		}
	}
	return nil
}

// FileContext records what happened to a file when a lease was released,
// including the actual file content so cross-machine agents can sync.
type FileContext struct {
	Path       string `json:"path"`
	Hostname   string `json:"hostname"`
	Plan       string `json:"plan"`
	Result     string `json:"result"`
	WorkID     string `json:"work_id"`
	Content    string `json:"content,omitempty"`
	ReleasedAt string `json:"released_at"`
}

func (s *Store) fileContextKey(teamID, path string) string {
	return fmt.Sprintf("team:%s:file_context:%s", teamID, path)
}

// SetFileContext stores what an agent did to a file, for the next acquirer.
func (s *Store) SetFileContext(ctx context.Context, teamID string, fc FileContext) error {
	data, err := json.Marshal(fc)
	if err != nil {
		return err
	}
	return s.redis.Set(ctx, s.fileContextKey(teamID, fc.Path), data, time.Hour).Err()
}

// GetFileContexts returns stored file context for paths that have it.
func (s *Store) GetFileContexts(ctx context.Context, teamID string, paths []string) []FileContext {
	var results []FileContext
	for _, p := range paths {
		data, err := s.redis.Get(ctx, s.fileContextKey(teamID, p)).Bytes()
		if err != nil {
			continue
		}
		var fc FileContext
		if json.Unmarshal(data, &fc) == nil {
			results = append(results, fc)
		}
	}
	return results
}
