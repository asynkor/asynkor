package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestValidate_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/internal/validate-key" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("X-Internal-Token") != "test-token" {
			t.Errorf("missing or wrong internal token")
		}

		var body validateKeyRequest
		json.NewDecoder(r.Body).Decode(&body)
		if body.APIKey != "cf_live_testkey" {
			t.Errorf("unexpected api_key: %s", body.APIKey)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(validateKeyResponse{
			TeamID:   "550e8400-e29b-41d4-a716-446655440000",
			TeamSlug: "acme",
			Plan:     "team",
			Config: validateKeyConfig{
				LeaseTTL:          300,
				HeartbeatInterval: 60,
				ConflictMode:      "warn",
				IgnorePatterns:    []string{"dist/*"},
				AllowForceRelease: false,
			},
		})
	}))
	defer srv.Close()

	v := NewValidator(srv.URL, "test-token")
	ctx, err := v.Validate("cf_live_testkey")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if ctx.TeamID != "550e8400-e29b-41d4-a716-446655440000" {
		t.Errorf("wrong team_id: %s", ctx.TeamID)
	}
	if ctx.TeamSlug != "acme" {
		t.Errorf("wrong team_slug: %s", ctx.TeamSlug)
	}
	if ctx.ConflictMode != "warn" {
		t.Errorf("wrong conflict_mode: %s", ctx.ConflictMode)
	}
	if len(ctx.IgnorePatterns) != 1 || ctx.IgnorePatterns[0] != "dist/*" {
		t.Errorf("wrong ignore_patterns: %v", ctx.IgnorePatterns)
	}
}

func TestValidate_InvalidKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid_key"}`))
	}))
	defer srv.Close()

	v := NewValidator(srv.URL, "test-token")
	_, err := v.Validate("cf_live_bad")
	if err == nil {
		t.Fatal("expected error for invalid key")
	}
}

func TestValidate_WrongInternalToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Internal-Token") != "correct-token" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(validateKeyResponse{TeamID: "x", Config: validateKeyConfig{LeaseTTL: 300}})
	}))
	defer srv.Close()

	v := NewValidator(srv.URL, "wrong-token")
	_, err := v.Validate("cf_live_any")
	if err == nil {
		t.Fatal("expected error for wrong internal token")
	}
}

func TestValidate_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"internal"}`))
	}))
	defer srv.Close()

	v := NewValidator(srv.URL, "test-token")
	_, err := v.Validate("cf_live_any")
	if err == nil {
		t.Fatal("expected error for server error")
	}
}

func TestValidate_Unreachable(t *testing.T) {
	v := NewValidator("http://127.0.0.1:19999", "test-token")
	_, err := v.Validate("cf_live_any")
	if err == nil {
		t.Fatal("expected error when server is unreachable")
	}
}

func TestValidate_NilIgnorePatterns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(validateKeyResponse{
			TeamID:   "id1",
			TeamSlug: "test",
			Plan:     "solo",
			Config:   validateKeyConfig{LeaseTTL: 300, HeartbeatInterval: 60, ConflictMode: "warn"},
		})
	}))
	defer srv.Close()

	v := NewValidator(srv.URL, "tok")
	ctx, err := v.Validate("key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ctx.IgnorePatterns == nil {
		t.Fatal("IgnorePatterns should never be nil")
	}
	if len(ctx.IgnorePatterns) != 0 {
		t.Fatalf("expected empty slice, got %v", ctx.IgnorePatterns)
	}
}
