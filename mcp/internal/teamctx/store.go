package teamctx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"asynkor/mcp/internal/work"
)

type Rule struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Paths       []string `json:"paths"`
	Severity    string   `json:"severity"`
}

type Memory struct {
	ID           string   `json:"id"`
	Content      string   `json:"content"`
	Paths        []string `json:"paths"`
	Tags         []string `json:"tags"`
	Source       string   `json:"source"`
	AgentSession string   `json:"agentSession"`
	CreatedAt    string   `json:"createdAt"`
}

type Zone struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Paths        []string `json:"paths"`
	Sensitivity  string   `json:"sensitivity"`
	Action       string   `json:"action"`
	Instructions string   `json:"instructions"`
}

type CompletedWork struct {
	ID           string   `json:"id"`
	Plan         string   `json:"plan"`
	Result       string   `json:"result"`
	Hostname     string   `json:"hostname"`
	Learnings    string   `json:"learnings,omitempty"`
	Decisions    string   `json:"decisions,omitempty"`
	FilesTouched []string `json:"files_touched,omitempty"`
	CompletedAt  string   `json:"completed_at"`
}

type PersistentFollowup struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Area        string `json:"area,omitempty"`
	Priority    string `json:"priority"`
	Type        string `json:"type,omitempty"`
	Context     string `json:"context,omitempty"`
	Tried       string `json:"tried,omitempty"`
	WatchOut    string `json:"watch_out,omitempty"`
	ParentPlan  string `json:"parent_plan,omitempty"`
}

type TeamContext struct {
	Rules         []Rule               `json:"rules"`
	Memories      []Memory             `json:"memories"`
	Zones         []Zone               `json:"zones"`
	RecentWorks   []CompletedWork      `json:"recent_works,omitempty"`
	OpenFollowups []PersistentFollowup `json:"open_followups,omitempty"`
}

type Store struct {
	javaURL       string
	internalToken string
	client        *http.Client

	mu    sync.RWMutex
	cache map[string]*cachedContext
}

type cachedContext struct {
	ctx       *TeamContext
	fetchedAt time.Time
}

// cacheTTL is intentionally short so the "every agent inherits the team's
// brain the moment it joins" promise is not a lie of degree. Agent-driven
// invalidation (via CreateMemory below) clears the entry on the local
// instance, but dashboard-driven memory creation has no NATS hook yet, so
// the worst-case staleness window is bounded by this constant.
const cacheTTL = 5 * time.Second

func NewStore(javaURL, internalToken string) *Store {
	return &Store{
		javaURL:       javaURL,
		internalToken: internalToken,
		client:        &http.Client{Timeout: 5 * time.Second},
		cache:         make(map[string]*cachedContext),
	}
}

// SetCacheForTest pre-populates the in-memory cache for a team. It exists
// so handler tests can inject rules / zones / memories without standing up
// a fake Java HTTP server. The entry uses the current timestamp so it
// stays valid for cacheTTL.
func (s *Store) SetCacheForTest(teamID string, tc *TeamContext) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache[teamID] = &cachedContext{ctx: tc, fetchedAt: time.Now()}
}

func (s *Store) Get(ctx context.Context, teamID string) *TeamContext {
	s.mu.RLock()
	if c, ok := s.cache[teamID]; ok && time.Since(c.fetchedAt) < cacheTTL {
		s.mu.RUnlock()
		return c.ctx
	}
	s.mu.RUnlock()

	tc, err := s.fetch(teamID)
	if err != nil {
		log.Printf("teamctx: fetch error for %s: %v", teamID, err)
		return &TeamContext{}
	}

	s.mu.Lock()
	if len(s.cache) > 1000 {
		now := time.Now()
		for k, v := range s.cache {
			if now.Sub(v.fetchedAt) > time.Hour {
				delete(s.cache, k)
			}
		}
	}
	s.cache[teamID] = &cachedContext{ctx: tc, fetchedAt: time.Now()}
	s.mu.Unlock()
	return tc
}

func (s *Store) fetch(teamID string) (*TeamContext, error) {
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/internal/teams/%s/context", s.javaURL, url.PathEscape(teamID)), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Internal-Token", s.internalToken)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch context: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("fetch context %d: %s", resp.StatusCode, body)
	}

	var tc TeamContext
	if err := json.NewDecoder(resp.Body).Decode(&tc); err != nil {
		return nil, fmt.Errorf("decode context: %w", err)
	}
	return &tc, nil
}

func (s *Store) CreateMemory(teamID, content string, paths, tags []string, agentSession string) error {
	body := map[string]any{
		"content":       content,
		"paths":         paths,
		"tags":          tags,
		"agent_session": agentSession,
	}
	data, _ := json.Marshal(body)

	req, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("%s/internal/teams/%s/memories", s.javaURL, url.PathEscape(teamID)),
		bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", s.internalToken)

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("create memory: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("create memory %d: %s", resp.StatusCode, b)
	}

	s.mu.Lock()
	delete(s.cache, teamID)
	s.mu.Unlock()

	return nil
}

func (s *Store) PersistWork(teamID string, w *work.Work) error {
	followups := make([]map[string]any, len(w.Followups))
	for i, f := range w.Followups {
		followups[i] = map[string]any{
			"id":          f.ID,
			"description": f.Description,
			"area":        f.Area,
			"priority":    f.Priority,
			"type":        f.Type,
			"context":     f.Context,
			"tried":       f.Tried,
			"watch_out":   f.WatchOut,
		}
	}

	var completedAt string
	if w.CompletedAt != nil {
		completedAt = w.CompletedAt.Format(time.RFC3339)
	}

	body := map[string]any{
		"work_id":       w.ID,
		"session_id":    w.SessionID,
		"hostname":      w.Hostname,
		"plan":          w.Plan,
		"result":        w.Result,
		"learnings":     w.Learnings,
		"decisions":     w.Decisions,
		"files_touched": w.FilesTouched,
		"followups":     followups,
		"created_at":    w.CreatedAt.Format(time.RFC3339),
		"completed_at":  completedAt,
	}
	data, _ := json.Marshal(body)

	req, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("%s/internal/teams/%s/works", s.javaURL, url.PathEscape(teamID)),
		bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", s.internalToken)

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("persist work: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("persist work %d: %s", resp.StatusCode, b)
	}

	return nil
}

func (s *Store) TakeFollowup(teamID, followupID, workID string) error {
	body := map[string]any{
		"work_id": workID,
	}
	data, _ := json.Marshal(body)

	req, err := http.NewRequest(http.MethodPatch,
		fmt.Sprintf("%s/internal/teams/%s/followups/%s/take", s.javaURL, url.PathEscape(teamID), url.PathEscape(followupID)),
		bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", s.internalToken)

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("take followup: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("take followup %d: %s", resp.StatusCode, b)
	}

	return nil
}

type RelevantContext struct {
	ActiveWork []CompletedWork      `json:"active_work,omitempty"`
	RecentWork []CompletedWork      `json:"recent_work,omitempty"`
	Decisions  []string             `json:"decisions,omitempty"`
	Learnings  []string             `json:"learnings,omitempty"`
	Followups  []PersistentFollowup `json:"followups,omitempty"`
}

func (s *Store) GetRelevantContext(teamID string, paths []string) (*RelevantContext, error) {
	u := fmt.Sprintf("%s/internal/teams/%s/context/relevant?paths=%s",
		s.javaURL, url.PathEscape(teamID), url.QueryEscape(strings.Join(paths, ",")))

	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Internal-Token", s.internalToken)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get relevant context: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get relevant context %d: %s", resp.StatusCode, b)
	}

	var rc RelevantContext
	if err := json.NewDecoder(resp.Body).Decode(&rc); err != nil {
		return nil, fmt.Errorf("decode relevant context: %w", err)
	}
	return &rc, nil
}
