package auth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

type cachedAuth struct {
	key     *KeyContext
	expires time.Time
}

type Validator struct {
	javaURL       string
	internalToken string
	client        *http.Client

	mu    sync.RWMutex
	cache map[string]*cachedAuth
}

const authCacheTTL = 60 * time.Second

func NewValidator(javaURL, internalToken string) *Validator {
	return &Validator{
		javaURL:       javaURL,
		internalToken: internalToken,
		client:        &http.Client{Timeout: 5 * time.Second},
		cache:         make(map[string]*cachedAuth),
	}
}

type validateKeyRequest struct {
	APIKey string `json:"api_key"`
}

type validateKeyConfig struct {
	LeaseTTL          int      `json:"lease_ttl"`
	HeartbeatInterval int      `json:"heartbeat_interval"`
	ConflictMode      string   `json:"conflict_mode"`
	IgnorePatterns    []string `json:"ignore_patterns"`
	AllowForceRelease bool     `json:"allow_force_release"`
}

type validateKeyTeam struct {
	TeamID   string            `json:"team_id"`
	TeamSlug string            `json:"team_slug"`
	Plan     string            `json:"plan"`
	Config   validateKeyConfig `json:"config"`
}

type validateKeyResponse struct {
	// Team-scoped shape (legacy): team fields on the envelope.
	Scope    string            `json:"scope"`
	TeamID   string            `json:"team_id"`
	TeamSlug string            `json:"team_slug"`
	Plan     string            `json:"plan"`
	Config   validateKeyConfig `json:"config"`

	// User-scoped shape: list of accessible teams + default.
	UserID        string            `json:"user_id"`
	Teams         []validateKeyTeam `json:"teams"`
	DefaultTeamID string            `json:"default_team_id"`
}

// Validate returns the full KeyContext. For team-scoped keys Teams has one
// entry and DefaultTeamID matches it; for user-scoped keys Teams lists every
// team the user can access.
func (v *Validator) Validate(apiKey string) (*KeyContext, error) {
	// Check cache first.
	v.mu.RLock()
	if c, ok := v.cache[apiKey]; ok && time.Now().Before(c.expires) {
		v.mu.RUnlock()
		return c.key, nil
	}
	v.mu.RUnlock()

	reqBody, _ := json.Marshal(validateKeyRequest{APIKey: apiKey})

	req, err := http.NewRequest(http.MethodPost, v.javaURL+"/internal/validate-key", bytes.NewReader(reqBody))
	if err != nil {
		log.Printf("auth: create request error: %v", err)
		return nil, fmt.Errorf("authentication failed")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", v.internalToken)

	resp, err := v.client.Do(req)
	if err != nil {
		log.Printf("auth: validate key request error: %v", err)
		return nil, fmt.Errorf("authentication failed")
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("invalid_key")
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("auth validation failed: status=%d body=%s", resp.StatusCode, body)
		return nil, fmt.Errorf("authentication failed")
	}

	var result validateKeyResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Printf("auth: decode response error: %v", err)
		return nil, fmt.Errorf("authentication failed")
	}

	kc := &KeyContext{Scope: result.Scope}
	if kc.Scope == "" {
		// Legacy response (pre-V9) has no scope field → treat as team-scoped.
		kc.Scope = "team"
	}

	if kc.Scope == "user" {
		kc.UserID = result.UserID
		kc.DefaultTeamID = result.DefaultTeamID
		for _, t := range result.Teams {
			kc.Teams = append(kc.Teams, teamFromResponse(t))
		}
		if len(kc.Teams) == 0 {
			return nil, fmt.Errorf("authentication failed")
		}
		if kc.DefaultTeamID == "" {
			kc.DefaultTeamID = kc.Teams[0].TeamID
		}
	} else {
		tc := teamFromResponse(validateKeyTeam{
			TeamID:   result.TeamID,
			TeamSlug: result.TeamSlug,
			Plan:     result.Plan,
			Config:   result.Config,
		})
		kc.Teams = []*TeamContext{tc}
		kc.DefaultTeamID = tc.TeamID
	}

	// Cache successful validation.
	v.mu.Lock()
	if len(v.cache) > 10000 {
		// Evict expired entries first.
		now := time.Now()
		for k, c := range v.cache {
			if now.After(c.expires) {
				delete(v.cache, k)
			}
		}
		// If still over limit, delete the oldest 1000 entries.
		if len(v.cache) > 10000 {
			type entry struct {
				key     string
				expires time.Time
			}
			entries := make([]entry, 0, len(v.cache))
			for k, c := range v.cache {
				entries = append(entries, entry{key: k, expires: c.expires})
			}
			// Sort by expiry ascending (oldest first).
			for i := 0; i < len(entries); i++ {
				for j := i + 1; j < len(entries); j++ {
					if entries[j].expires.Before(entries[i].expires) {
						entries[i], entries[j] = entries[j], entries[i]
					}
				}
			}
			toDelete := 1000
			if toDelete > len(entries) {
				toDelete = len(entries)
			}
			for _, e := range entries[:toDelete] {
				delete(v.cache, e.key)
			}
		}
	}
	v.cache[apiKey] = &cachedAuth{key: kc, expires: time.Now().Add(authCacheTTL)}
	v.mu.Unlock()

	return kc, nil
}

func teamFromResponse(t validateKeyTeam) *TeamContext {
	patterns := t.Config.IgnorePatterns
	if patterns == nil {
		patterns = []string{}
	}
	return &TeamContext{
		TeamID:            t.TeamID,
		TeamSlug:          t.TeamSlug,
		Plan:              t.Plan,
		HeartbeatInterval: t.Config.HeartbeatInterval,
		ConflictMode:      t.Config.ConflictMode,
		IgnorePatterns:    patterns,
	}
}
