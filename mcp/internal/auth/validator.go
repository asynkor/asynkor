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
	team    *TeamContext
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

type validateKeyResponse struct {
	TeamID   string            `json:"team_id"`
	TeamSlug string            `json:"team_slug"`
	Plan     string            `json:"plan"`
	Config   validateKeyConfig `json:"config"`
}

func (v *Validator) Validate(apiKey string) (*TeamContext, error) {
	// Check cache first.
	v.mu.RLock()
	if c, ok := v.cache[apiKey]; ok && time.Now().Before(c.expires) {
		v.mu.RUnlock()
		return c.team, nil
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

	patterns := result.Config.IgnorePatterns
	if patterns == nil {
		patterns = []string{}
	}

	tc := &TeamContext{
		TeamID:            result.TeamID,
		TeamSlug:          result.TeamSlug,
		Plan:              result.Plan,
		HeartbeatInterval: result.Config.HeartbeatInterval,
		ConflictMode:      result.Config.ConflictMode,
		IgnorePatterns:    patterns,
	}

	// Cache successful validation.
	v.mu.Lock()
	v.cache[apiKey] = &cachedAuth{team: tc, expires: time.Now().Add(authCacheTTL)}
	v.mu.Unlock()

	return tc, nil
}
