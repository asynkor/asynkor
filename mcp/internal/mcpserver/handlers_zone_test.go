package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"asynkor/mcp/internal/teamctx"
)

// injectZones pre-populates the teamctx cache for the test team. The fake
// teamctx Store points at an unreachable URL in newTestServer, so without
// this seed every Get() returns an empty TeamContext.
func injectZones(t *testing.T, srv *Server, zones ...teamctx.Zone) {
	t.Helper()
	srv.teamCtx.SetCacheForTest("team-test", &teamctx.TeamContext{
		Zones:    zones,
		Memories: []teamctx.Memory{{ID: "seed", Content: "test seed memory"}},
	})
}

// TestZone_BlockRefusesWithoutAck verifies the headline claim of the new
// narrative: "protected zones for the parts that matter most." A zone with
// action=block must actually refuse a asynkor_start that touches it.
func TestZone_BlockRefusesWithoutAck(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	injectZones(t, srv, teamctx.Zone{
		ID:          "zone-auth",
		Name:        "Auth core",
		Paths:       []string{"src/auth/**"},
		Sensitivity: "critical",
		Action:      "block",
	})

	ctx := ctxForMode("sess-A", "warn")
	res := callStartWithArgs(t, srv, ctx, map[string]any{
		"plan":  "rewrite jwt verification",
		"paths": "src/auth/jwt.ts",
	})

	var body map[string]any
	if err := json.Unmarshal([]byte(resultText(t, res)), &body); err != nil {
		t.Fatalf("not json: %v", err)
	}
	if body["ok"] != false {
		t.Fatalf("expected ok=false, got %v", body)
	}
	if body["error"] != "zone_block" {
		t.Errorf("expected error=zone_block, got %v", body["error"])
	}
	zones, _ := body["zones"].([]any)
	if len(zones) != 1 {
		t.Fatalf("expected 1 zone in payload, got %d", len(zones))
	}
	entry := zones[0].(map[string]any)
	if entry["zone_id"] != "zone-auth" || entry["action"] != "block" {
		t.Errorf("zone payload incorrect: %v", entry)
	}
}

// TestZone_BlockProceedsWithAck verifies the override path: passing
// acknowledge_zone with the zone IDs lets the start proceed and surfaces
// the zone in warnings.protected_zones so the agent still knows about it.
func TestZone_BlockProceedsWithAck(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	injectZones(t, srv, teamctx.Zone{
		ID:     "zone-auth",
		Name:   "Auth core",
		Paths:  []string{"src/auth/**"},
		Action: "block",
	})

	ctx := ctxForMode("sess-A", "warn")
	res := callStartWithArgs(t, srv, ctx, map[string]any{
		"plan":             "rewrite jwt verification — coordinated with security lead",
		"paths":            "src/auth/jwt.ts",
		"acknowledge_zone": "zone-auth",
	})

	var body map[string]any
	_ = json.Unmarshal([]byte(resultText(t, res)), &body)
	if body["ok"] != true {
		t.Fatalf("expected ok=true after ack, got %v", body)
	}
	warnings, ok := body["warnings"].(map[string]any)
	if !ok {
		t.Fatalf("expected warnings object after ack, got %v", body["warnings"])
	}
	zones, _ := warnings["protected_zones"].([]any)
	if len(zones) != 1 {
		t.Errorf("expected acknowledged zone to still appear in warnings, got %d", len(zones))
	}
}

// TestZone_ConfirmRequiresAck mirrors TestZone_BlockRefusesWithoutAck but
// for action=confirm. The error string and message differ ("user
// confirmation" vs "BLOCK") but the gating mechanic is the same.
func TestZone_ConfirmRequiresAck(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	injectZones(t, srv, teamctx.Zone{
		ID:     "zone-billing",
		Name:   "Billing logic",
		Paths:  []string{"src/billing/**"},
		Action: "confirm",
	})

	ctx := ctxForMode("sess-A", "warn")
	res := callStartWithArgs(t, srv, ctx, map[string]any{
		"plan":  "tweak proration math",
		"paths": "src/billing/proration.ts",
	})

	var body map[string]any
	_ = json.Unmarshal([]byte(resultText(t, res)), &body)
	if body["ok"] != false {
		t.Fatalf("expected ok=false, got %v", body)
	}
	if body["error"] != "zone_confirm" {
		t.Errorf("expected error=zone_confirm, got %v", body["error"])
	}
}

// TestZone_PartialAckStillBlocks verifies that acknowledging only one of
// two blocking zones is not enough — the agent must surface every blocking
// zone before proceeding. Mirrors TestOverlap_BlockModePartialAckStillBlocks.
func TestZone_PartialAckStillBlocks(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	injectZones(t, srv,
		teamctx.Zone{ID: "z-auth", Name: "Auth", Paths: []string{"src/auth/**"}, Action: "block"},
		teamctx.Zone{ID: "z-pay", Name: "Payments", Paths: []string{"src/payments/**"}, Action: "block"},
	)

	ctx := ctxForMode("sess-A", "warn")
	res := callStartWithArgs(t, srv, ctx, map[string]any{
		"plan":             "cross-cutting refactor",
		"paths":            "src/auth/jwt.ts,src/payments/charge.ts",
		"acknowledge_zone": "z-auth", // only ack one
	})

	var body map[string]any
	_ = json.Unmarshal([]byte(resultText(t, res)), &body)
	if body["ok"] != false {
		t.Fatalf("expected ok=false on partial ack, got %v", body)
	}
}

// TestZone_WarnSurfaceWithoutBlocking verifies action=warn lets the start
// proceed but surfaces the zone in the response warnings.
func TestZone_WarnSurfaceWithoutBlocking(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	injectZones(t, srv, teamctx.Zone{
		ID:     "z-config",
		Name:   "Config files",
		Paths:  []string{"config/**"},
		Action: "warn",
	})

	ctx := ctxForMode("sess-A", "warn")
	res := callStartWithArgs(t, srv, ctx, map[string]any{
		"plan":  "tighten redis timeout",
		"paths": "config/redis.yml",
	})

	var body map[string]any
	_ = json.Unmarshal([]byte(resultText(t, res)), &body)
	if body["ok"] != true {
		t.Fatalf("expected ok=true for warn-zone, got %v", body)
	}
	warnings, _ := body["warnings"].(map[string]any)
	zones, _ := warnings["protected_zones"].([]any)
	if len(zones) != 1 {
		t.Errorf("expected warn zone to surface in warnings, got %d", len(zones))
	}
}

// TestZone_AllowDoesNotInterfere verifies action=allow is a true no-op.
func TestZone_AllowDoesNotInterfere(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	injectZones(t, srv, teamctx.Zone{
		ID:     "z-docs",
		Name:   "Docs",
		Paths:  []string{"docs/**"},
		Action: "allow",
	})

	ctx := ctxForMode("sess-A", "warn")
	res := callStartWithArgs(t, srv, ctx, map[string]any{
		"plan":  "fix typo in README",
		"paths": "docs/README.md",
	})

	var body map[string]any
	_ = json.Unmarshal([]byte(resultText(t, res)), &body)
	if body["ok"] != true {
		t.Fatalf("expected ok=true for allow zone, got %v", body)
	}
	if _, has := body["warnings"]; has {
		t.Errorf("allow zone should not produce warnings: %v", body["warnings"])
	}
}

// TestZone_NoPlannedPathsSkipsEnforcement guards against the foot-gun where
// an agent calls asynkor_start without paths and we have no idea which
// zones it would touch. We must NOT block in that case (no paths = unknown,
// not "touches every zone"); the warn-mode plan-text fallback is what
// catches this kind of vague start.
func TestZone_NoPlannedPathsSkipsEnforcement(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	injectZones(t, srv, teamctx.Zone{
		ID:     "z-auth",
		Paths:  []string{"src/auth/**"},
		Action: "block",
	})

	ctx := ctxForMode("sess-A", "warn")
	res := callStartWithArgs(t, srv, ctx, map[string]any{
		"plan": "vague plan with no paths declared",
	})

	var body map[string]any
	_ = json.Unmarshal([]byte(resultText(t, res)), &body)
	if body["ok"] != true {
		t.Fatalf("expected ok=true with no planned paths, got %v", body)
	}
}

// TestCheck_ZoneSurfacesEnforcement verifies that asynkor_check tells the
// agent what enforcement will happen at start time, not just that a zone
// exists. Before the change, the check output displayed zones but never
// hinted that they were actually enforced.
func TestCheck_ZoneSurfacesEnforcement(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	injectZones(t, srv,
		teamctx.Zone{ID: "z-block", Name: "Block zone", Paths: []string{"src/auth/**"}, Action: "block"},
		teamctx.Zone{ID: "z-warn", Name: "Warn zone", Paths: []string{"src/billing/**"}, Action: "warn"},
		teamctx.Zone{ID: "z-confirm", Name: "Confirm zone", Paths: []string{"src/payments/**"}, Action: "confirm"},
	)

	for _, tc := range []struct {
		path string
		want string
	}{
		{"src/auth/jwt.ts", "REFUSED"},
		{"src/billing/proration.ts", "warnings.protected_zones"},
		{"src/payments/charge.ts", "user confirmation"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			ctx := ctxForMode("sess-check", "warn")
			req := mcp.CallToolRequest{}
			req.Params.Name = "asynkor_check"
			req.Params.Arguments = map[string]any{"paths": tc.path}
			res, err := srv.handleCheck(ctx, req)
			if err != nil {
				t.Fatalf("handleCheck: %v", err)
			}
			text := resultText(t, res)
			if !strings.Contains(text, tc.want) {
				t.Errorf("check on %s should mention %q, got:\n%s", tc.path, tc.want, text)
			}
			if !strings.Contains(text, "ENFORCEMENT:") {
				t.Errorf("check should explicitly say ENFORCEMENT:, got:\n%s", text)
			}
		})
	}
}
