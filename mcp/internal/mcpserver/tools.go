package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/mark3labs/mcp-go/mcp"

	"asynkor/mcp/internal/auth"
	"asynkor/mcp/internal/lease"
	"asynkor/mcp/internal/teamctx"
	"asynkor/mcp/internal/work"
)

var uuidRegex = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func isValidUUID(s string) bool {
	return uuidRegex.MatchString(s)
}

func relativeTime(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		if m == 1 {
			return "1 minute ago"
		}
		return fmt.Sprintf("%d minutes ago", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		if h == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", h)
	case d < 7*24*time.Hour:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1 day ago"
		}
		return fmt.Sprintf("%d days ago", days)
	default:
		weeks := int(d.Hours() / 24 / 7)
		if weeks == 1 {
			return "1 week ago"
		}
		return fmt.Sprintf("%d weeks ago", weeks)
	}
}

func toolJSON(v any) *mcp.CallToolResult {
	data, _ := json.Marshal(v)
	return mcp.NewToolResultText(string(data))
}

func toolError(msg string) *mcp.CallToolResult {
	return mcp.NewToolResultError(msg)
}

func (s *Server) handleBriefing(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if e := getAuthError(ctx); e != "" {
		return toolError("unauthorized: " + e), nil
	}
	team := getTeam(ctx)

	// Auto-cleanup stale work items older than 2 hours on every briefing.
	if cleaned, _ := s.works.CleanupStale(ctx, team.TeamID, 2*time.Hour); cleaned > 0 {
		log.Printf("briefing: cleaned %d stale work items for team %s", cleaned, team.TeamSlug)
	}

	active, _ := s.works.ListActive(ctx, team.TeamID)

	// Auto-park orphaned work items whose sessions have expired.
	// Grace period: don't park work started within the last 10 minutes —
	// the agent may just be busy editing (session key expires after 3 min
	// of no tool calls, but the SSE connection and work are still alive).
	liveSessions, _ := s.sessions.List(ctx, team.TeamID)
	liveSessionIDs := make(map[string]bool, len(liveSessions))
	for _, sess := range liveSessions {
		liveSessionIDs[sess.ID] = true
	}
	var liveActive []*work.Work
	for _, w := range active {
		if liveSessionIDs[w.SessionID] {
			liveActive = append(liveActive, w)
		} else if time.Since(w.CreatedAt) > 10*time.Minute {
			// Session dead AND work is old enough — safe to auto-park.
			if _, err := s.works.Park(ctx, team.TeamID, w.ID, "", "auto-parked: session expired"); err == nil {
				log.Printf("briefing: auto-parked orphaned work %s (session %s expired)", w.ID, w.SessionID)
			}
		} else {
			// Session dead but work is recent — keep it active, agent
			// is likely still editing and will call a tool soon.
			liveActive = append(liveActive, w)
		}
	}
	active = liveActive

	recent, _ := s.works.ListRecent(ctx, team.TeamID, 5)
	followups, sources := s.works.AllOpenFollowups(ctx, team.TeamID)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Team: %s\n", team.TeamSlug))

	// Multi-team switcher surface. Only rendered for user-scoped keys that
	// can see more than one team; team-scoped keys keep the briefing clean.
	if kc := getKey(ctx); kc != nil && kc.Scope == "user" && len(kc.Teams) > 1 {
		sb.WriteString(fmt.Sprintf("\nAccessible teams (%d) — call asynkor_switch_team to change the active team:\n", len(kc.Teams)))
		for _, t := range kc.Teams {
			marker := "  "
			if t.TeamID == team.TeamID {
				marker = "→ "
			}
			sb.WriteString(fmt.Sprintf("%s%s (%s)\n", marker, t.TeamSlug, t.TeamID))
		}
	}

	tc := s.teamCtx.Get(ctx, team.TeamID)

	// Long-term project context — the owner-curated project brain. Shown
	// first so every agent inherits it before anything else.
	pc := tc.ProjectContext
	if pc != nil && pc.Instructions != "" {
		sb.WriteString("\nOwner instructions for long-term context:\n")
		sb.WriteString(pc.Instructions + "\n")
	}
	if pc != nil && pc.Content != "" {
		sb.WriteString(fmt.Sprintf("\nLong-term project context (v%d", pc.Version))
		if pc.UpdatedAt != "" {
			sb.WriteString(", updated " + relativeTime(pc.UpdatedAt))
		}
		sb.WriteString("):\n")
		sb.WriteString(pc.Content)
		if !strings.HasSuffix(pc.Content, "\n") {
			sb.WriteString("\n")
		}
		sb.WriteString("\nTo update: call asynkor_context_update with the new full content and a short summary. Follow the owner instructions above.\n")
	}

	// CONTEXT REQUIRED fires when both the long-term doc AND the memory
	// staging feed are empty — brand-new team with nothing to inherit.
	longTermEmpty := pc == nil || pc.Content == ""
	contextRequired := longTermEmpty && len(tc.Memories) == 0

	if contextRequired {
		sb.WriteString("\n⚠ CONTEXT REQUIRED: Long-term context is empty. Before starting any work, scan the codebase (README, directory structure, key config files, recent git history) and populate the long-term project context — either by calling asynkor_context_update with the full initial doc, or by capturing individual insights via asynkor_remember.\n")
	}

	if len(active) == 0 {
		sb.WriteString("\nNo active work.\n")
	} else {
		sb.WriteString(fmt.Sprintf("\nActive work (%d):\n", len(active)))
		for _, w := range active {
			elapsed := time.Since(w.CreatedAt).Round(time.Minute)
			sb.WriteString(fmt.Sprintf("• %s — %s (started %s ago)\n", w.Hostname, w.Plan, elapsed))
		}
	}

	activeLeases, _ := s.leases.ListAll(ctx, team.TeamID)
	if len(activeLeases) > 0 {
		sb.WriteString(fmt.Sprintf("\nActive leases (%d):\n", len(activeLeases)))
		for _, l := range activeLeases {
			elapsed := time.Since(l.AcquiredAt).Round(time.Second)
			sb.WriteString(fmt.Sprintf("• %s — held by %s for %q (%s ago)\n", l.Path, l.Hostname, l.Plan, elapsed))
		}
	}

	parked, _ := s.works.ListParked(ctx, team.TeamID)
	if len(parked) > 0 {
		sb.WriteString(fmt.Sprintf("\nParked work — available for pickup (%d):\n", len(parked)))
		for _, w := range parked {
			sb.WriteString(fmt.Sprintf("• %s — %s\n", w.Hostname, w.Plan))
			sb.WriteString(fmt.Sprintf("  handoff_id: %s\n", w.ID))
			if w.Progress != "" {
				sb.WriteString(fmt.Sprintf("  Progress: %s\n", w.Progress))
			}
			if w.ParkedNotes != "" {
				sb.WriteString(fmt.Sprintf("  Notes: %s\n", w.ParkedNotes))
			}
			if w.Learnings != "" {
				sb.WriteString(fmt.Sprintf("  Learnings: %s\n", w.Learnings))
			}
			if w.Decisions != "" {
				sb.WriteString(fmt.Sprintf("  Decisions: %s\n", w.Decisions))
			}
			if len(w.FilesTouched) > 0 {
				sb.WriteString(fmt.Sprintf("  Files: %s\n", strings.Join(w.FilesTouched, ", ")))
			}
		}
	}

	var completedInRecent []*work.Work
	for _, w := range recent {
		if w.Status == "completed" {
			completedInRecent = append(completedInRecent, w)
		}
	}
	if len(completedInRecent) > 0 {
		sb.WriteString(fmt.Sprintf("\nRecently completed (%d):\n", len(completedInRecent)))
		for _, w := range completedInRecent {
			elapsed := time.Since(*w.CompletedAt).Round(time.Minute)
			sb.WriteString(fmt.Sprintf("• %s — %s (completed %s ago)\n", w.Hostname, w.Plan, elapsed))
			if w.Result != "" {
				sb.WriteString(fmt.Sprintf("  Result: %s\n", w.Result))
			}
			if len(w.Followups) > 0 {
				sb.WriteString(fmt.Sprintf("  Followups: %d tasks\n", len(w.Followups)))
			}
		}
	}

	if len(followups) > 0 {
		sb.WriteString(fmt.Sprintf("\nOpen follow-ups (%d):\n", len(followups)))
		for i, f := range followups {
			priority := f.Priority
			if priority == "" {
				priority = "normal"
			}
			line := fmt.Sprintf("• [%s] %s", priority, f.Description)
			if f.Area != "" {
				line += fmt.Sprintf(" — %s", f.Area)
			}
			sb.WriteString(line + "\n")
			if f.ID != "" {
				sb.WriteString(fmt.Sprintf("  followup_id: %s\n", f.ID))
			}
			if i < len(sources) {
				sb.WriteString(fmt.Sprintf("  from: %s\n", sources[i]))
			}
		}
	} else {
		sb.WriteString("\nNo open follow-ups.\n")
	}

	if len(tc.Rules) > 0 {
		sb.WriteString(fmt.Sprintf("\nTeam rules (%d):\n", len(tc.Rules)))
		for _, r := range tc.Rules {
			sb.WriteString(fmt.Sprintf("• [%s] %s\n", r.Severity, r.Title))
			sb.WriteString(fmt.Sprintf("  %s\n", r.Description))
			if len(r.Paths) > 0 {
				sb.WriteString(fmt.Sprintf("  applies to: %s\n", strings.Join(r.Paths, ", ")))
			}
		}
	}

	if len(tc.Zones) > 0 {
		sb.WriteString(fmt.Sprintf("\nProtected zones (%d):\n", len(tc.Zones)))
		for _, z := range tc.Zones {
			sb.WriteString(fmt.Sprintf("• %s [%s → %s]\n", z.Name, z.Sensitivity, z.Action))
			if len(z.Paths) > 0 {
				sb.WriteString(fmt.Sprintf("  paths: %s\n", strings.Join(z.Paths, ", ")))
			}
			if z.Instructions != "" {
				sb.WriteString(fmt.Sprintf("  note: %s\n", z.Instructions))
			}
		}
	}

	if len(tc.Memories) > 0 {
		limit := len(tc.Memories)
		if limit > 10 {
			limit = 10
		}
		sb.WriteString(fmt.Sprintf("\nTeam memory (showing %d of %d):\n", limit, len(tc.Memories)))
		for _, m := range tc.Memories[:limit] {
			sb.WriteString(fmt.Sprintf("• [id %s] %s\n", m.ID, m.Content))
			if len(m.Paths) > 0 {
				sb.WriteString(fmt.Sprintf("  files: %s\n", strings.Join(m.Paths, ", ")))
			}
			if len(m.Tags) > 0 {
				sb.WriteString(fmt.Sprintf("  tags: %s\n", strings.Join(m.Tags, ", ")))
			}
		}
		sb.WriteString("\nMemory entries are short-term staging. If a learning is durable, asynkor_context_update merges it into the long-term project context — then asynkor_forget the staging entry by id.\n")
	}

	if len(tc.RecentWorks) > 0 {
		sb.WriteString(fmt.Sprintf("\nProject history (persistent, %d):\n", len(tc.RecentWorks)))
		for _, w := range tc.RecentWorks {
			sb.WriteString(fmt.Sprintf("  [%s] %s — %s\n", relativeTime(w.CompletedAt), w.Hostname, w.Plan))
			if w.Result != "" {
				sb.WriteString(fmt.Sprintf("    Result: %s\n", w.Result))
			}
			if w.Decisions != "" {
				sb.WriteString(fmt.Sprintf("    Decision: %s\n", w.Decisions))
			}
			if w.Learnings != "" {
				sb.WriteString(fmt.Sprintf("    Learned: %s\n", w.Learnings))
			}
			if len(w.FilesTouched) > 0 {
				sb.WriteString(fmt.Sprintf("    Files: %s\n", strings.Join(w.FilesTouched, ", ")))
			}
		}
	}

	if len(tc.OpenFollowups) > 0 {
		sb.WriteString(fmt.Sprintf("\nOpen follow-ups (persistent, %d):\n", len(tc.OpenFollowups)))
		for _, f := range tc.OpenFollowups {
			priority := f.Priority
			if priority == "" {
				priority = "normal"
			}
			line := fmt.Sprintf("  [%s] %s", priority, f.Description)
			if f.Area != "" {
				line += fmt.Sprintf(" — %s", f.Area)
			}
			sb.WriteString(line + "\n")
			if f.ParentPlan != "" {
				sb.WriteString(fmt.Sprintf("    from: %s\n", f.ParentPlan))
			}
			if f.Context != "" {
				sb.WriteString(fmt.Sprintf("    Context: %s\n", f.Context))
			}
			if f.Tried != "" {
				sb.WriteString(fmt.Sprintf("    Tried: %s\n", f.Tried))
			}
			if f.WatchOut != "" {
				sb.WriteString(fmt.Sprintf("    Watch out: %s\n", f.WatchOut))
			}
		}
	}

	// Threaded messaging — surface anything addressed to me (work, host, or
	// team broadcast). Top 3 only; full list via asynkor_inbox. Resolve
	// my work_id via session lookup; falls back to "" which means we still
	// see host- and team-scoped threads.
	hostname := getHostname(ctx)
	sessID := getSessionID(ctx)
	var myWorkID string
	if w, _ := s.works.GetBySession(ctx, team.TeamID, sessID); w != nil {
		myWorkID = w.ID
	}
	if inbox, err := s.teamCtx.ThreadInbox(team.TeamID, myWorkID, hostname); err == nil && len(inbox) > 0 {
		sb.WriteString(fmt.Sprintf("\nInbox — threads addressed to you (%d):\n", len(inbox)))
		limit := len(inbox)
		if limit > 3 {
			limit = 3
		}
		for _, t := range inbox[:limit] {
			scope := t.TargetKind + ":" + t.TargetValue
			sb.WriteString(fmt.Sprintf("• [%s] %s — %s (from %s, %s)\n",
				t.Status, t.Topic, scope, t.OpenerHost, relativeTime(t.UpdatedAt)))
			sb.WriteString(fmt.Sprintf("  thread_id: %s\n", t.ID))
		}
		if len(inbox) > 3 {
			sb.WriteString(fmt.Sprintf("  …and %d more — call asynkor_inbox for the full list.\n", len(inbox)-3))
		}
		sb.WriteString("Use asynkor_thread(thread_id) to read, asynkor_reply(thread_id, body[, close=true]) to respond.\n")
	}

	return mcp.NewToolResultText(sb.String()), nil
}

func (s *Server) handleStart(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if e := getAuthError(ctx); e != "" {
		return toolError("unauthorized: " + e), nil
	}
	team := getTeam(ctx)
	sessID := getSessionID(ctx)
	hostname := getHostname(ctx)

	plan, err := req.RequireString("plan")
	if err != nil {
		return toolError("plan is required"), nil
	}
	if len(plan) > 10000 {
		return toolError("plan must be 10000 characters or less"), nil
	}

	// Refuse if long-term context is empty — force the agent to scan first.
	// Only enforce when we can confirm the context was fetched (team has rules
	// or zones or prior work, proving the Java backend is reachable). Long-term
	// context is the project_context_versions doc; memories are a staging feed
	// that also counts so existing teams aren't suddenly blocked.
	tcCtxEarly := s.teamCtx.Get(ctx, team.TeamID)
	contextFetched := len(tcCtxEarly.Rules) > 0 || len(tcCtxEarly.Zones) > 0 || len(tcCtxEarly.Memories) > 0 || len(tcCtxEarly.RecentWorks) > 0 || (tcCtxEarly.ProjectContext != nil && tcCtxEarly.ProjectContext.Content != "")
	longTermEmpty := tcCtxEarly.ProjectContext == nil || tcCtxEarly.ProjectContext.Content == ""
	if contextFetched && longTermEmpty && len(tcCtxEarly.Memories) == 0 {
		return toolJSON(map[string]any{
			"ok":               false,
			"error":            "context_required",
			"context_required": true,
			"message":          "Long-term context is empty. Before starting work, scan the codebase (README, directory structure, key config files, recent git history) and populate the project context — call asynkor_context_update with the initial doc, or asynkor_remember for individual insights. Then retry asynkor_start.",
		}), nil
	}

	plannedPaths := parseCSV(getArgString(req, "paths", ""))
	if len(plannedPaths) > 200 {
		return toolError("paths must contain 200 entries or fewer"), nil
	}

	// Resume parked work if handoff_id is provided.
	handoffID := getArgString(req, "handoff_id", "")
	if handoffID != "" {
		if !isValidUUID(handoffID) {
			return toolError("invalid handoff_id format"), nil
		}
		resumed, err := s.works.ResumeParked(ctx, team.TeamID, handoffID, sessID, hostname)
		if err != nil {
			log.Printf("ERROR resume parked: %v", err)
			return toolError("failed to resume parked work — it may have expired or been picked up"), nil
		}
		if len(plannedPaths) > 0 {
			resumed.PlannedPaths = plannedPaths
		}

		// Acquire leases on the resumed work's paths.
		if len(resumed.PlannedPaths) > 0 {
			acquired, blocked, _ := s.leases.AcquireMany(ctx, team.TeamID, resumed.PlannedPaths, resumed.ID, sessID, hostname, resumed.Plan)
			_ = acquired
			if len(blocked) > 0 {
				blockedPaths := make([]string, len(blocked))
				for i, b := range blocked {
					blockedPaths[i] = b.Path
				}
				resp := map[string]any{
					"ok":           true,
					"work_id":      resumed.ID,
					"resumed_from": handoffID,
					"message":      fmt.Sprintf("Resumed parked work. BLOCKED: %d file(s) are leased — you MUST call asynkor_lease_wait before editing: %s.", len(blocked), strings.Join(blockedPaths, ", ")),
					"blocked_files": blockedPayload(blocked),
					"action_required": map[string]any{
						"type":        "lease_wait",
						"paths":       blockedPaths,
						"instruction": "Call asynkor_lease_wait with these paths before editing them. After acquiring, RE-READ each file — the previous holder may have changed them.",
					},
				}
				return toolJSON(resp), nil
			}
		}

		s.nats.Publish(team.TeamID, "work.started", resumed)
		return toolJSON(map[string]any{
			"ok":           true,
			"work_id":      resumed.ID,
			"resumed_from": handoffID,
			"message":      fmt.Sprintf("Resumed parked work: '%s'", resumed.Plan),
		}), nil
	}

	ackSet := parseWorkIDSet(getArgString(req, "acknowledge_overlap", ""))
	zoneAckSet := parseWorkIDSet(getArgString(req, "acknowledge_zone", ""))

	// Overlap detection: compare the new plan against every *other* session's
	// active work. Same-session matches are not flagged because calling
	// asynkor_start twice in the same session is a deliberate pivot.
	active, _ := s.works.ListActive(ctx, team.TeamID)
	otherActive := make([]*work.Work, 0, len(active))
	for _, w := range active {
		if w.SessionID != sessID {
			otherActive = append(otherActive, w)
		}
	}

	var overlaps []overlapEntry
	for _, w := range otherActive {
		if reason := detectOverlap(plan, plannedPaths, w); reason != "" {
			overlaps = append(overlaps, overlapEntry{Work: w, Reason: reason})
		}
	}

	conflictMode := team.ConflictMode
	if conflictMode == "" {
		conflictMode = "warn"
	}

	// Block mode: refuse unless the caller has acknowledged every conflicting
	// work ID explicitly. This forces the agent to surface the overlap to the
	// user and get a deliberate go-ahead before proceeding.
	if conflictMode == "block" && len(overlaps) > 0 && !allAcknowledged(overlaps, ackSet) {
		s.recordConflictEvent(ctx, team.TeamID, "", sessID, hostname, plan, overlaps, "block", false)
		return toolJSON(map[string]any{
			"ok":        false,
			"error":     "overlap",
			"message":   "Cannot start: teammates are already working on related things. Coordinate with them, then pass acknowledge_overlap with a comma-separated list of the conflicting work_ids to proceed.",
			"conflicts": conflictPayload(overlaps),
		}), nil
	}

	// Zone enforcement: match the planned paths against the team's protected
	// zones and apply each zone's action. Only meaningful when the agent
	// declared paths up front — without paths we cannot tell which zones the
	// work would touch. Block / confirm zones require zone_id acknowledgement;
	// warn zones surface as warnings but proceed; allow is a no-op.
	tcCtx := s.teamCtx.Get(ctx, team.TeamID)
	zoneHits := matchedZonesForPaths(plannedPaths, tcCtx.Zones)
	blockingZones := zoneHits.requiringAck()
	if len(blockingZones) > 0 && !allZonesAcknowledged(blockingZones, zoneAckSet) {
		strongest := blockingZones[0].Zone.Action
		for _, h := range blockingZones {
			if h.Zone.Action == "block" {
				strongest = "block"
				break
			}
		}
		msg := "Cannot start: planned paths fall inside a zone that requires confirmation. Surface the zone(s) below to the user, get explicit go-ahead, and retry with acknowledge_zone listing every zone_id."
		if strongest == "block" {
			msg = "Cannot start: planned paths fall inside a BLOCK zone. This area is protected — coordinate with the team owner, then if the work is genuinely required, retry with acknowledge_zone listing every zone_id."
		}
		return toolJSON(map[string]any{
			"ok":      false,
			"error":   "zone_" + strongest,
			"message": msg,
			"zones":   zonePayload(blockingZones),
		}), nil
	}

	w, err := s.works.Start(ctx, team.TeamID, sessID, hostname, plan, plannedPaths)
	if err != nil {
		log.Printf("ERROR start work: %v", err)
		return toolError("failed to start work"), nil
	}

	// Auto-acquire leases on declared paths.
	var leaseBlocked []lease.BlockedLease
	if len(plannedPaths) > 0 {
		_, leaseBlocked, _ = s.leases.AcquireMany(ctx, team.TeamID, plannedPaths, w.ID, sessID, hostname, plan)
	}

	if followupID := getArgString(req, "followup_id", ""); followupID != "" {
		if !isValidUUID(followupID) {
			return toolError("invalid followup_id format"), nil
		}
		if err := s.works.TakeFollowup(ctx, team.TeamID, followupID, w.ID); err != nil {
			if errors.Is(err, work.ErrFollowupAlreadyTaken) {
				takerWorkID, _ := s.works.FollowupTaker(ctx, team.TeamID, followupID)
				return toolJSON(map[string]any{
					"ok":            false,
					"error":         "followup_taken",
					"message":       fmt.Sprintf("This followup has already been claimed by work %s. Coordinate with that agent, or pick a different followup.", takerWorkID),
					"taken_by_work": takerWorkID,
				}), nil
			}
			log.Printf("ERROR take followup: %v", err)
			return toolError("failed to take followup"), nil
		}
		go func() {
			if err := s.teamCtx.TakeFollowup(team.TeamID, followupID, w.ID); err != nil {
				log.Printf("ERROR mark followup taken in postgres: %v", err)
			}
		}()
	}

	s.nats.Publish(team.TeamID, "work.started", w)

	if len(overlaps) > 0 {
		s.recordConflictEvent(ctx, team.TeamID, w.ID, sessID, hostname, plan, overlaps, conflictMode, len(ackSet) > 0)
	}

	resp := map[string]any{
		"ok":               true,
		"work_id":          w.ID,
		"active_teammates": teammatesPayload(otherActive),
	}
	warnings := map[string]any{}
	if len(overlaps) > 0 {
		warnings["active_overlap"] = conflictPayload(overlaps)
	}
	if len(leaseBlocked) > 0 {
		warnings["blocked_leases"] = blockedPayload(leaseBlocked)
		// Surface blocked paths explicitly so the agent can't miss them.
		blockedPaths := make([]string, len(leaseBlocked))
		for i, b := range leaseBlocked {
			blockedPaths[i] = b.Path
		}
		resp["action_required"] = map[string]any{
			"type":        "lease_wait",
			"paths":       blockedPaths,
			"instruction": "Call asynkor_lease_wait with these paths before editing them. After acquiring, RE-READ each file — the previous holder may have changed them.",
		}
	}
	// warn-action zones don't refuse the start but should always be visible
	// in the response. If block/confirm zones were acknowledged via
	// acknowledge_zone, surface them too — the agent should still know what
	// it just stepped into.
	zoneWarnings := zoneHits.warnings()
	if len(blockingZones) > 0 {
		zoneWarnings = append(zoneWarnings, blockingZones...)
	}
	if len(zoneWarnings) > 0 {
		warnings["protected_zones"] = zonePayload(zoneWarnings)
	}
	if len(warnings) > 0 {
		resp["warnings"] = warnings
		// Build a composable message with the most critical info first.
		var msgParts []string
		if len(leaseBlocked) > 0 {
			blockedPaths := make([]string, len(leaseBlocked))
			for i, b := range leaseBlocked {
				blockedPaths[i] = b.Path
			}
			msgParts = append(msgParts, fmt.Sprintf("BLOCKED: %d file(s) are leased by other agents — you MUST call asynkor_lease_wait before editing: %s", len(leaseBlocked), strings.Join(blockedPaths, ", ")))
		}
		if len(overlaps) > 0 {
			msgParts = append(msgParts, fmt.Sprintf("%d teammate(s) are working on related things — review warnings.active_overlap and coordinate", len(overlaps)))
		}
		if len(zoneWarnings) > 0 {
			msgParts = append(msgParts, fmt.Sprintf("planned paths touch %d protected zone(s) — review warnings.protected_zones", len(zoneWarnings)))
		}
		resp["message"] = fmt.Sprintf("Work started (work_id: %s — save this, pass to asynkor_finish if session reconnects). ", w.ID) + strings.Join(msgParts, ". ") + "."
	} else {
		resp["message"] = fmt.Sprintf("Work started (work_id: %s — save this, pass to asynkor_finish if session reconnects). Your teammates can now see: '%s'", w.ID, plan)
	}
	return toolJSON(resp), nil
}

func (s *Server) handleFinish(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if e := getAuthError(ctx); e != "" {
		return toolError("unauthorized: " + e), nil
	}
	team := getTeam(ctx)
	sessID := getSessionID(ctx)

	result, err := req.RequireString("result")
	if err != nil {
		return toolError("result is required"), nil
	}
	if len(result) > 50000 {
		return toolError("result must be 50000 characters or less"), nil
	}

	learnings := getArgString(req, "learnings", "")
	if len(learnings) > 5000 {
		return toolError("learnings must be 5000 characters or less"), nil
	}
	decisions := getArgString(req, "decisions", "")
	if len(decisions) > 5000 {
		return toolError("decisions must be 5000 characters or less"), nil
	}
	filesTouchedRaw := getArgString(req, "files_touched", "")
	var filesTouched []string
	if filesTouchedRaw != "" {
		for _, p := range strings.Split(filesTouchedRaw, ",") {
			if t := strings.TrimSpace(p); t != "" {
				if len(t) > 4096 {
					continue
				}
				filesTouched = append(filesTouched, filepath.Clean(t))
			}
		}
	}

	// file_snapshots: JSON object mapping path → file content.
	// Stored on the server so the next agent to acquire these files
	// gets the actual content — critical for cross-machine coordination.
	fileSnapshotsRaw := getArgString(req, "file_snapshots", "")
	var fileSnapshots map[string]string
	if fileSnapshotsRaw != "" {
		if len(fileSnapshotsRaw) > 100*1024*1024 {
			return toolError("file_snapshots too large (max 100MB)"), nil
		}
		if err := json.Unmarshal([]byte(fileSnapshotsRaw), &fileSnapshots); err != nil {
			return toolError("file_snapshots must be a JSON object mapping file paths to content strings"), nil
		}
		for path, content := range fileSnapshots {
			if len(content) > 10*1024*1024 {
				return toolError(fmt.Sprintf("file snapshot for %s too large (max 10MB per file)", path)), nil
			}
		}
	}

	followupsRaw := getArgString(req, "followups", "")
	var followups []work.Followup
	if followupsRaw != "" {
		if err := json.Unmarshal([]byte(followupsRaw), &followups); err != nil {
			return toolError("followups must be a valid JSON array"), nil
		}
		if len(followups) > 20 {
			return toolError("followups array must have at most 20 elements"), nil
		}
	}

	current, err := s.works.GetBySession(ctx, team.TeamID, sessID)
	if err != nil {
		log.Printf("ERROR retrieving work by session: %v", err)
	}
	// Fallback: if session→work mapping broke (proxy reconnect), try work_id param.
	if current == nil {
		if wid := getArgString(req, "work_id", ""); wid != "" && isValidUUID(wid) {
			current, err = s.works.GetByID(ctx, team.TeamID, wid)
			if err != nil {
				log.Printf("ERROR retrieving work by ID: %v", err)
			}
		}
	}
	if current == nil {
		return toolError("no active work found for this session. Your session likely reconnected and lost its binding. Pass work_id (from the asynkor_start response) to recover: asynkor_finish(work_id=\"<your-work-id>\", result=\"...\")"), nil
	}

	completed, err := s.works.Complete(ctx, team.TeamID, current.ID, result, followups)
	if err != nil {
		log.Printf("ERROR complete work: %v", err)
		return toolError("failed to complete work"), nil
	}

	completed.Learnings = learnings
	completed.Decisions = decisions
	completed.FilesTouched = filesTouched

	// If this session was resumed from a parked handoff, cancel the original
	// handoff so it stops appearing in future briefings under "Parked work —
	// available for pickup". Without this, every resumed-then-finished session
	// leaks a stale handoff row forever.
	if current.HandoffFrom != "" {
		if err := s.works.Cancel(ctx, team.TeamID, current.HandoffFrom); err != nil {
			log.Printf("WARN cancel originating handoff %s on finish: %v", current.HandoffFrom, err)
		}
	}

	if err := s.leases.ReleaseByWork(ctx, team.TeamID, current.ID); err != nil {
		log.Printf("WARN release leases on finish: %v", err)
	}

	// Store file context (and snapshot content if provided) for each
	// touched file. The next agent to acquire these files gets the actual
	// content — critical for cross-machine coordination.
	for _, path := range filesTouched {
		fc := lease.FileContext{
			Path:       path,
			Hostname:   completed.Hostname,
			Plan:       completed.Plan,
			Result:     result,
			WorkID:     completed.ID,
			Content:    fileSnapshots[path],
			ReleasedAt: time.Now().UTC().Format(time.RFC3339),
		}
		_ = s.leases.SetFileContext(ctx, team.TeamID, fc)
	}

	s.nats.Publish(team.TeamID, "work.completed", completed)

	go func() {
		if err := s.teamCtx.PersistWork(team.TeamID, completed); err != nil {
			log.Printf("ERROR persist work to postgres: %v", err)
		}
	}()

	return toolJSON(map[string]any{
		"ok":        true,
		"work_id":   completed.ID,
		"followups": len(followups),
		"message":   fmt.Sprintf("Work completed and shared with team. %d follow-up(s) recorded.", len(followups)),
	}), nil
}

func (s *Server) handleCheck(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if e := getAuthError(ctx); e != "" {
		return toolError("unauthorized: " + e), nil
	}
	team := getTeam(ctx)

	pathsRaw, err := req.RequireString("paths")
	if err != nil {
		return toolError("paths is required"), nil
	}

	var paths []string
	for _, p := range strings.Split(pathsRaw, ",") {
		if t := strings.TrimSpace(p); t != "" {
			if len(t) > 4096 {
				continue
			}
			paths = append(paths, filepath.Clean(t))
		}
	}
	if len(paths) == 0 {
		return toolError("paths must contain at least one file path"), nil
	}

	tc := s.teamCtx.Get(ctx, team.TeamID)

	var matchedRules []teamctx.Rule
	for _, r := range tc.Rules {
		for _, p := range paths {
			if pathMatchesAny(p, r.Paths) {
				matchedRules = append(matchedRules, r)
				break
			}
		}
	}

	var matchedZones []teamctx.Zone
	for _, z := range tc.Zones {
		for _, p := range paths {
			if pathMatchesAny(p, z.Paths) {
				matchedZones = append(matchedZones, z)
				break
			}
		}
	}

	var matchedMemories []teamctx.Memory
	for _, m := range tc.Memories {
		for _, p := range paths {
			if pathMatchesAny(p, m.Paths) {
				matchedMemories = append(matchedMemories, m)
				break
			}
		}
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Context for: %s\n", strings.Join(paths, ", ")))

	leaseStatus, _ := s.leases.CheckPaths(ctx, team.TeamID, paths)
	sessID := getSessionID(ctx)
	if len(leaseStatus) > 0 {
		sb.WriteString(fmt.Sprintf("\nLeased files (%d):\n", len(leaseStatus)))
		for p, l := range leaseStatus {
			holder := "another agent"
			if l.SessionID == sessID {
				holder = "you"
			}
			elapsed := time.Since(l.AcquiredAt).Round(time.Second)
			sb.WriteString(fmt.Sprintf("• %s — held by %s (%s, %s ago) for %q\n", p, holder, l.Hostname, elapsed, l.Plan))
			if l.SessionID != sessID {
				sb.WriteString("  ACTION: Wait for this file to be released before editing. Use asynkor_lease_wait to block until free, then re-read the file before making changes.\n")
			}
		}
	}

	// Match against both declared-future paths (PlannedPaths, set at
	// asynkor_start) and already-touched paths (FilesTouched, set at
	// asynkor_finish). Before the PlannedPaths change, active work was
	// effectively invisible to conflict detection because FilesTouched was
	// empty until the work was complete.
	active, _ := s.works.ListActive(ctx, team.TeamID)
	pathSet := make(map[string]bool, len(paths))
	for _, p := range paths {
		pathSet[p] = true
	}
	var conflicting []*work.Work
	for _, w := range active {
		if w.SessionID == sessID {
			continue
		}
		matched := false
		for _, pp := range w.PlannedPaths {
			if pathSet[pp] {
				matched = true
				break
			}
		}
		if !matched {
			for _, ft := range w.FilesTouched {
				if pathSet[ft] {
					matched = true
					break
				}
			}
		}
		if matched {
			conflicting = append(conflicting, w)
		}
	}
	if len(conflicting) > 0 {
		sb.WriteString(fmt.Sprintf("\nActive work on these files (%d):\n", len(conflicting)))
		for _, w := range conflicting {
			elapsed := time.Since(w.CreatedAt).Round(time.Minute)
			sb.WriteString(fmt.Sprintf("• %s — %s (started %s ago)\n", w.Hostname, w.Plan, elapsed))
			if len(w.PlannedPaths) > 0 {
				sb.WriteString(fmt.Sprintf("  plans to touch: %s\n", strings.Join(w.PlannedPaths, ", ")))
			}
		}
	}

	if len(matchedZones) > 0 {
		sb.WriteString(fmt.Sprintf("\nProtected zones (%d):\n", len(matchedZones)))
		for _, z := range matchedZones {
			sb.WriteString(fmt.Sprintf("• %s [%s → %s]\n", z.Name, z.Sensitivity, z.Action))
			if z.ID != "" {
				sb.WriteString(fmt.Sprintf("  zone_id: %s\n", z.ID))
			}
			if z.Instructions != "" {
				sb.WriteString(fmt.Sprintf("  %s\n", z.Instructions))
			}
			switch z.Action {
			case "block":
				sb.WriteString("  ENFORCEMENT: asynkor_start with these paths will be REFUSED unless you pass acknowledge_zone with this zone_id.\n")
			case "confirm":
				sb.WriteString("  ENFORCEMENT: asynkor_start with these paths requires user confirmation — pass acknowledge_zone with this zone_id after getting explicit go-ahead.\n")
			case "warn":
				sb.WriteString("  ENFORCEMENT: asynkor_start will proceed but the response will surface this zone in warnings.protected_zones.\n")
			}
		}
	}

	if len(matchedRules) > 0 {
		sb.WriteString(fmt.Sprintf("\nRules (%d):\n", len(matchedRules)))
		for _, r := range matchedRules {
			sb.WriteString(fmt.Sprintf("• [%s] %s — %s\n", r.Severity, r.Title, r.Description))
		}
	}

	if len(matchedMemories) > 0 {
		sb.WriteString(fmt.Sprintf("\nMemory (%d):\n", len(matchedMemories)))
		for _, m := range matchedMemories {
			sb.WriteString(fmt.Sprintf("• %s\n", m.Content))
		}
	}

	relevant, err := s.teamCtx.GetRelevantContext(team.TeamID, paths)
	if err != nil {
		log.Printf("WARN get relevant context: %v", err)
	} else {
		if len(relevant.RecentWork) > 0 {
			sb.WriteString(fmt.Sprintf("\nRecent work history (%d):\n", len(relevant.RecentWork)))
			for _, w := range relevant.RecentWork {
				sb.WriteString(fmt.Sprintf("• [%s] %s — %s\n", relativeTime(w.CompletedAt), w.Hostname, w.Plan))
				if w.Decisions != "" {
					sb.WriteString(fmt.Sprintf("  Decision: %s\n", w.Decisions))
				}
				if w.Learnings != "" {
					sb.WriteString(fmt.Sprintf("  Learned: %s\n", w.Learnings))
				}
			}
		}

		if len(relevant.Followups) > 0 {
			sb.WriteString(fmt.Sprintf("\nRelated follow-ups (%d):\n", len(relevant.Followups)))
			for _, f := range relevant.Followups {
				priority := f.Priority
				if priority == "" {
					priority = "normal"
				}
				sb.WriteString(fmt.Sprintf("• [%s] %s\n", priority, f.Description))
				if f.WatchOut != "" {
					sb.WriteString(fmt.Sprintf("  Watch out: %s\n", f.WatchOut))
				}
			}
		}
	}

	if len(matchedZones) == 0 && len(matchedRules) == 0 && len(matchedMemories) == 0 &&
		len(conflicting) == 0 && (relevant == nil || (len(relevant.RecentWork) == 0 && len(relevant.Followups) == 0)) {
		sb.WriteString("\nNo rules, zones, memories, or history apply to these paths.")
	}

	return mcp.NewToolResultText(sb.String()), nil
}

func (s *Server) handleRemember(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if e := getAuthError(ctx); e != "" {
		return toolError("unauthorized: " + e), nil
	}
	team := getTeam(ctx)
	sessID := getSessionID(ctx)

	content, err := req.RequireString("content")
	if err != nil {
		return toolError("content is required"), nil
	}
	if len(content) > 10000 {
		return toolError("content must be 10000 characters or less"), nil
	}

	pathsRaw := getArgString(req, "paths", "")
	var paths []string
	if pathsRaw != "" {
		for _, p := range strings.Split(pathsRaw, ",") {
			if t := strings.TrimSpace(p); t != "" {
				if len(t) > 4096 {
					continue
				}
				paths = append(paths, filepath.Clean(t))
			}
		}
	}

	tagsRaw := getArgString(req, "tags", "")
	var tags []string
	if tagsRaw != "" {
		for _, t := range strings.Split(tagsRaw, ",") {
			if t := strings.TrimSpace(t); t != "" {
				tags = append(tags, t)
			}
		}
	}

	if err := s.teamCtx.CreateMemory(team.TeamID, content, paths, tags, sessID); err != nil {
		log.Printf("ERROR save memory: %v", err)
		return toolError(fmt.Sprintf("failed to save memory: %v", err)), nil
	}

	return toolJSON(map[string]any{
		"ok":      true,
		"message": "Memory saved. All teammates will see this in their next briefing.",
	}), nil
}

func (s *Server) handleForget(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if e := getAuthError(ctx); e != "" {
		return toolError("unauthorized: " + e), nil
	}
	team := getTeam(ctx)

	memoryID, err := req.RequireString("memory_id")
	if err != nil {
		return toolError("memory_id is required (find it in asynkor_briefing under 'Team memory' — the [id …] prefix on each entry)"), nil
	}
	memoryID = strings.TrimSpace(memoryID)
	if !isValidUUID(memoryID) {
		return toolError("memory_id must be a UUID — copy it from the [id …] prefix in asynkor_briefing output"), nil
	}

	if err := s.teamCtx.DeleteMemory(team.TeamID, memoryID); err != nil {
		log.Printf("ERROR forget memory %s: %v", memoryID, err)
		return toolError(fmt.Sprintf("failed to forget memory: %v", err)), nil
	}

	return toolJSON(map[string]any{
		"ok":      true,
		"message": fmt.Sprintf("Memory %s deleted. The team memory list shrinks by one — every future briefing reflects this.", memoryID),
	}), nil
}

func pathMatchesAny(filePath string, patterns []string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, pattern := range patterns {
		if matched, _ := filepath.Match(pattern, filePath); matched {
			return true
		}
		if strings.Contains(pattern, "**") {
			simple := strings.ReplaceAll(pattern, "**", "*")
			if matched, _ := filepath.Match(simple, filePath); matched {
				return true
			}
		}
		if strings.HasPrefix(filePath, strings.TrimSuffix(pattern, "/**")) {
			return true
		}
		if strings.HasPrefix(filePath, strings.TrimSuffix(pattern, "/*")) {
			return true
		}
	}
	return false
}

func argsMap(req mcp.CallToolRequest) map[string]any {
	if m, ok := req.Params.Arguments.(map[string]any); ok {
		return m
	}
	return nil
}

func getArgString(req mcp.CallToolRequest, key, defaultVal string) string {
	if m := argsMap(req); m != nil {
		if v, ok := m[key]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	return defaultVal
}

// ─── Overlap detection for asynkor_start ─────────────────────────────────────

type overlapEntry struct {
	Work   *work.Work
	Reason string
}

// overlapJaccardThreshold is the minimum Jaccard similarity of significant
// plan words for two work items to be considered overlapping. Empirically
// "refresh token rotation for auth" vs "adding refresh token to auth module"
// scores ~0.5; unrelated plans score near 0.
const overlapJaccardThreshold = 0.3

// stopwords is a small list of high-frequency English filler words that
// carry no signal for engineering plan similarity. Earlier versions of this
// list also stripped verbs like "add", "fix", "update", "change" — but those
// are actually high-signal in engineering plans ("add jwt rotation" vs
// "add database migration" should both keep the verb so the noun phrases
// dominate the Jaccard score, not collapse to nothing). Audit feedback:
// removing them caused unrelated short plans to both score ~0% and miss
// real overlaps.
var stopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "from": true,
	"that": true, "this": true, "into": true, "have": true, "been": true,
	"will": true, "what": true, "when": true, "where": true, "some": true,
	"just": true, "more": true, "also": true, "then": true, "than": true,
	"them": true, "they": true, "their": true, "there": true, "here": true,
	"about": true, "over": true, "after": true, "before": true, "make": true,
	"work": true, "working": true, "code": true, "file": true, "files": true,
	"using": true, "use": true, "new": true,
}

// significantWords extracts the non-trivial lowercase word set from a string.
// Punctuation is stripped, words ≤ 3 chars or in stopwords are dropped.
func significantWords(s string) map[string]bool {
	out := make(map[string]bool)
	for _, raw := range strings.Fields(strings.ToLower(s)) {
		w := strings.TrimFunc(raw, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		})
		if len(w) <= 3 || stopwords[w] {
			continue
		}
		out[w] = true
	}
	return out
}

// jaccardSimilarity returns the Jaccard index over two word sets, in [0, 1].
func jaccardSimilarity(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for w := range a {
		if b[w] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// detectOverlap returns a non-empty human-readable reason if the new work
// should be flagged as overlapping with an existing active work, or "" if
// there is no overlap. Checks in order:
//  1. shared planned path (exact) — strongest signal
//  2. planned path overlaps the other's already-touched paths
//  3. plan-text Jaccard similarity ≥ overlapJaccardThreshold
func detectOverlap(newPlan string, newPaths []string, other *work.Work) string {
	if len(newPaths) > 0 {
		for _, p := range newPaths {
			for _, op := range other.PlannedPaths {
				if p == op {
					return "same planned path: " + p
				}
				// Glob match: "frontend/**" should match "frontend/src/foo.tsx"
				if pathMatchesAny(p, []string{op}) {
					return fmt.Sprintf("path matches planned glob %s: %s", op, p)
				}
				if pathMatchesAny(op, []string{p}) {
					return fmt.Sprintf("planned glob matches path %s: %s", p, op)
				}
			}
			for _, op := range other.FilesTouched {
				if p == op {
					return "path already touched: " + p
				}
				if pathMatchesAny(p, []string{op}) {
					return fmt.Sprintf("path matches touched glob %s: %s", op, p)
				}
			}
		}
	}

	sim := jaccardSimilarity(significantWords(newPlan), significantWords(other.Plan))
	if sim >= overlapJaccardThreshold {
		return fmt.Sprintf("similar plan text (%d%% word overlap)", int(sim*100))
	}
	return ""
}

func parseCSV(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if t := strings.TrimSpace(p); t != "" {
			if len(t) > 4096 {
				continue
			}
			out = append(out, filepath.Clean(t))
		}
	}
	return out
}

func parseWorkIDSet(raw string) map[string]bool {
	if raw == "" {
		return nil
	}
	out := make(map[string]bool)
	for _, id := range strings.Split(raw, ",") {
		if t := strings.TrimSpace(id); t != "" {
			out[t] = true
		}
	}
	return out
}

// allAcknowledged returns true iff every overlapping work ID is present in
// the caller-supplied acknowledgement set. Partial acknowledgement is not
// accepted — the agent must see all conflicts before proceeding.
func allAcknowledged(overlaps []overlapEntry, ack map[string]bool) bool {
	if len(overlaps) == 0 {
		return true
	}
	if len(ack) == 0 {
		return false
	}
	for _, o := range overlaps {
		if !ack[o.Work.ID] {
			return false
		}
	}
	return true
}

func conflictPayload(overlaps []overlapEntry) []map[string]any {
	out := make([]map[string]any, 0, len(overlaps))
	for _, o := range overlaps {
		entry := map[string]any{
			"work_id":     o.Work.ID,
			"hostname":    o.Work.Hostname,
			"plan":        o.Work.Plan,
			"started_ago": time.Since(o.Work.CreatedAt).Round(time.Minute).String(),
			"reason":      o.Reason,
		}
		if len(o.Work.PlannedPaths) > 0 {
			entry["planned_paths"] = o.Work.PlannedPaths
		}
		out = append(out, entry)
	}
	return out
}

func teammatesPayload(active []*work.Work) []map[string]any {
	out := make([]map[string]any, 0, len(active))
	for _, w := range active {
		entry := map[string]any{
			"work_id":     w.ID,
			"hostname":    w.Hostname,
			"plan":        w.Plan,
			"started_ago": time.Since(w.CreatedAt).Round(time.Minute).String(),
		}
		if len(w.PlannedPaths) > 0 {
			entry["planned_paths"] = w.PlannedPaths
		}
		out = append(out, entry)
	}
	return out
}

// ─── Zone enforcement for asynkor_start ──────────────────────────────────────

type zoneHit struct {
	Zone        teamctx.Zone
	MatchedPath string
}

type zoneHits []zoneHit

// matchedZonesForPaths returns one zoneHit per (zone, planned-path) match.
// A zone is hit at most once even if multiple planned paths fall inside it
// — duplicate notifications add no value for the agent.
func matchedZonesForPaths(plannedPaths []string, zones []teamctx.Zone) zoneHits {
	if len(plannedPaths) == 0 || len(zones) == 0 {
		return nil
	}
	out := make(zoneHits, 0, len(zones))
	for _, z := range zones {
		for _, p := range plannedPaths {
			if pathMatchesAny(p, z.Paths) {
				out = append(out, zoneHit{Zone: z, MatchedPath: p})
				break
			}
		}
	}
	return out
}

// requiringAck returns the subset of hits whose action is "block" or
// "confirm" — both must be acknowledged via acknowledge_zone before the
// start can proceed.
func (zh zoneHits) requiringAck() zoneHits {
	out := make(zoneHits, 0, len(zh))
	for _, h := range zh {
		if h.Zone.Action == "block" || h.Zone.Action == "confirm" {
			out = append(out, h)
		}
	}
	return out
}

// warnings returns the subset of hits whose action is "warn" — they should
// surface in the response but never refuse the start.
func (zh zoneHits) warnings() zoneHits {
	out := make(zoneHits, 0, len(zh))
	for _, h := range zh {
		if h.Zone.Action == "warn" {
			out = append(out, h)
		}
	}
	return out
}

// allZonesAcknowledged returns true iff every blocking zone hit has its
// zone ID in the caller-supplied acknowledgement set. Mirrors the
// allAcknowledged contract for overlap detection: partial ack is rejected
// so the agent always sees every zone before proceeding.
func allZonesAcknowledged(hits zoneHits, ack map[string]bool) bool {
	if len(hits) == 0 {
		return true
	}
	if len(ack) == 0 {
		return false
	}
	for _, h := range hits {
		if !ack[h.Zone.ID] {
			return false
		}
	}
	return true
}

func zonePayload(hits zoneHits) []map[string]any {
	out := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		entry := map[string]any{
			"zone_id":      h.Zone.ID,
			"name":         h.Zone.Name,
			"action":       h.Zone.Action,
			"sensitivity":  h.Zone.Sensitivity,
			"matched_path": h.MatchedPath,
		}
		if len(h.Zone.Paths) > 0 {
			entry["zone_paths"] = h.Zone.Paths
		}
		if h.Zone.Instructions != "" {
			entry["instructions"] = h.Zone.Instructions
		}
		out = append(out, entry)
	}
	return out
}

func (s *Server) recordConflictEvent(ctx context.Context, teamID, newWorkID, sessID, hostname, plan string, overlaps []overlapEntry, mode string, acknowledged bool) {
	now := time.Now().UTC().Format(time.RFC3339)
	var events []work.ConflictEvent
	for _, o := range overlaps {
		// Extract the conflicting path from the reason if possible,
		// otherwise use the reason text itself as the path description.
		path := o.Reason
		if idx := strings.Index(o.Reason, ": "); idx >= 0 {
			path = strings.TrimPrefix(o.Reason[idx+2:], "same planned path: ")
		}
		events = append(events, work.ConflictEvent{
			ID:                  fmt.Sprintf("%s-%s", newWorkID, o.Work.ID),
			Path:                path,
			RequestedBySession:  sessID,
			RequestedByAgent:    plan,
			RequestedByHostname: hostname,
			HeldBySession:       o.Work.SessionID,
			HeldByHostname:      o.Work.Hostname,
			Mode:                mode,
			DetectedAt:          now,
		})
	}
	if err := s.works.RecordConflict(ctx, teamID, events); err != nil {
		log.Printf("WARN record conflict: %v", err)
	}
}

func blockedPayload(blocked []lease.BlockedLease) []map[string]any {
	out := make([]map[string]any, 0, len(blocked))
	for _, b := range blocked {
		entry := map[string]any{"path": b.Path}
		if b.Holder != nil {
			entry["held_by"] = b.Holder.Hostname
			entry["held_for"] = b.Holder.Plan
			entry["held_since"] = time.Since(b.Holder.AcquiredAt).Round(time.Second).String()
		}
		out = append(out, entry)
	}
	return out
}

func (s *Server) handlePark(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if e := getAuthError(ctx); e != "" {
		return toolError("unauthorized: " + e), nil
	}
	team := getTeam(ctx)
	sessID := getSessionID(ctx)

	progress, err := req.RequireString("progress")
	if err != nil {
		return toolError("progress is required"), nil
	}
	if len(progress) > 10000 {
		return toolError("progress must be 10000 characters or less"), nil
	}
	notes := getArgString(req, "notes", "")
	learnings := getArgString(req, "learnings", "")
	decisions := getArgString(req, "decisions", "")
	filesTouched := parseCSV(getArgString(req, "files_touched", ""))

	current, err := s.works.GetBySession(ctx, team.TeamID, sessID)
	if err != nil {
		log.Printf("ERROR retrieving work by session: %v", err)
	}
	if current == nil {
		if wid := getArgString(req, "work_id", ""); wid != "" && isValidUUID(wid) {
			current, err = s.works.GetByID(ctx, team.TeamID, wid)
			if err != nil {
				log.Printf("ERROR retrieving work by ID: %v", err)
			}
		}
	}
	if current == nil {
		return toolError("no active work found for this session. Your session likely reconnected and lost its binding. Pass work_id (from the asynkor_start response) to recover: asynkor_park(work_id=\"<your-work-id>\", progress=\"...\")"), nil
	}

	parked, err := s.works.Park(ctx, team.TeamID, current.ID, progress, notes)
	if err != nil {
		log.Printf("ERROR park work: %v", err)
		return toolError("failed to park work"), nil
	}

	parked.Learnings = learnings
	parked.Decisions = decisions
	parked.FilesTouched = filesTouched

	if err := s.leases.ReleaseByWork(ctx, team.TeamID, current.ID); err != nil {
		log.Printf("WARN release leases on park: %v", err)
	}

	s.nats.Publish(team.TeamID, "work.parked", parked)

	go func() {
		if err := s.teamCtx.PersistWork(team.TeamID, parked); err != nil {
			log.Printf("ERROR persist parked work to postgres: %v", err)
		}
	}()

	return toolJSON(map[string]any{
		"ok":         true,
		"work_id":    parked.ID,
		"handoff_id": parked.ID,
		"message":    fmt.Sprintf("Work parked. Another agent can resume it via asynkor_start with handoff_id=%s", parked.ID),
	}), nil
}

func (s *Server) handleCancel(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if e := getAuthError(ctx); e != "" {
		return toolError("unauthorized: " + e), nil
	}
	team := getTeam(ctx)

	wid, err := req.RequireString("work_id")
	if err != nil {
		return toolError("work_id is required"), nil
	}
	if !isValidUUID(wid) {
		return toolError("invalid work_id format"), nil
	}

	// Release any leases held by this work.
	if err := s.leases.ReleaseByWork(ctx, team.TeamID, wid); err != nil {
		log.Printf("WARN release leases on cancel: %v", err)
	}

	if err := s.works.Cancel(ctx, team.TeamID, wid); err != nil {
		log.Printf("ERROR cancel work: %v", err)
		return toolError("failed to cancel work — it may have already expired"), nil
	}

	log.Printf("work cancelled: team=%s work=%s", team.TeamSlug, wid)
	return toolJSON(map[string]any{
		"ok":      true,
		"message": fmt.Sprintf("Work %s cancelled. Leases released, removed from active set and parked list.", wid),
	}), nil
}

func (s *Server) handleLeaseAcquire(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if e := getAuthError(ctx); e != "" {
		return toolError("unauthorized: " + e), nil
	}
	team := getTeam(ctx)
	sessID := getSessionID(ctx)
	hostname := getHostname(ctx)

	pathsRaw, err := req.RequireString("paths")
	if err != nil {
		return toolError("paths is required"), nil
	}
	paths := parseCSV(pathsRaw)
	if len(paths) == 0 {
		return toolError("paths must contain at least one file path"), nil
	}

	current, err := s.works.GetBySession(ctx, team.TeamID, sessID)
	if err != nil || current == nil {
		return toolError("no active work found — call asynkor_start first"), nil
	}

	acquired, blocked, err := s.leases.AcquireMany(ctx, team.TeamID, paths, current.ID, sessID, hostname, current.Plan)
	if err != nil {
		log.Printf("ERROR acquire leases: %v", err)
		return toolError("failed to acquire leases"), nil
	}

	resp := map[string]any{
		"ok":       len(blocked) == 0,
		"acquired": acquired,
	}
	if len(blocked) > 0 {
		resp["blocked"] = blockedPayload(blocked)
		resp["message"] = fmt.Sprintf("%d file(s) acquired, %d blocked by other agents. Use asynkor_lease_wait to block until free, then re-read the files before editing.", len(acquired), len(blocked))
	} else {
		resp["message"] = fmt.Sprintf("%d file(s) leased. Other agents will wait for you to finish.", len(acquired))
	}

	// Check for file context/snapshots on acquired files.
	if len(acquired) > 0 {
		if contexts := s.leases.GetFileContexts(ctx, team.TeamID, acquired); len(contexts) > 0 {
			snapshots := make(map[string]string)
			for _, fc := range contexts {
				if fc.Content != "" {
					snapshots[fc.Path] = fc.Content
				}
			}
			resp["file_changes"] = contexts
			if len(snapshots) > 0 {
				resp["file_snapshots"] = snapshots
				snapshotPaths := make([]string, 0, len(snapshots))
				for p := range snapshots {
					snapshotPaths = append(snapshotPaths, p)
				}
				existingMsg, _ := resp["message"].(string)
				resp["message"] = existingMsg + fmt.Sprintf(" SYNC REQUIRED: %d file(s) have updated content from another agent — WRITE the content from file_snapshots to your local files BEFORE editing: %s.",
					len(snapshots), strings.Join(snapshotPaths, ", "))
			}
		}
	}

	return toolJSON(resp), nil
}

func (s *Server) handleLeaseWait(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if e := getAuthError(ctx); e != "" {
		return toolError("unauthorized: " + e), nil
	}
	team := getTeam(ctx)
	sessID := getSessionID(ctx)
	hostname := getHostname(ctx)

	pathsRaw, err := req.RequireString("paths")
	if err != nil {
		return toolError("paths is required"), nil
	}
	paths := parseCSV(pathsRaw)
	if len(paths) == 0 {
		return toolError("paths must contain at least one file path"), nil
	}

	// Cap at 25s to fit within the MCP transport timeout (~30-60s).
	// If leases aren't free within this window, return still_blocked
	// and let the agent retry or work on other files.
	timeoutStr := getArgString(req, "timeout_seconds", "25")
	timeoutSec, err := strconv.Atoi(timeoutStr)
	if err != nil || timeoutSec < 0 {
		timeoutSec = 25
	}
	if timeoutSec > 30 {
		timeoutSec = 30
	}

	current, err := s.works.GetBySession(ctx, team.TeamID, sessID)
	if err != nil || current == nil {
		return toolError("no active work found — call asynkor_start first"), nil
	}

	timeout := time.Duration(timeoutSec) * time.Second
	acquired, blocked, err := s.leases.WaitAndAcquire(ctx, team.TeamID, paths, current.ID, sessID, hostname, current.Plan, timeout)
	if err != nil {
		log.Printf("ERROR lease wait: %v", err)
		return toolError("failed during lease wait"), nil
	}

	if len(blocked) > 0 {
		blockedPaths := make([]string, len(blocked))
		for i, b := range blocked {
			blockedPaths[i] = b.Path
		}
		return toolJSON(map[string]any{
			"ok":      false,
			"status":  "still_blocked",
			"blocked": blockedPayload(blocked),
			"message": fmt.Sprintf("Still blocked after %ds — %d file(s) held by other agents: %s. Work on other files first, then call asynkor_lease_wait again.", timeoutSec, len(blocked), strings.Join(blockedPaths, ", ")),
		}), nil
	}

	resp := map[string]any{
		"ok":       true,
		"acquired": acquired,
	}

	// Check for file context/snapshots from the previous holder.
	if contexts := s.leases.GetFileContexts(ctx, team.TeamID, acquired); len(contexts) > 0 {
		// Separate files with snapshots from files with only metadata.
		snapshots := make(map[string]string)
		var fileChanges []lease.FileContext
		for _, fc := range contexts {
			if fc.Content != "" {
				snapshots[fc.Path] = fc.Content
			}
			fileChanges = append(fileChanges, fc)
		}

		if len(snapshots) > 0 {
			resp["file_snapshots"] = snapshots
			snapshotPaths := make([]string, 0, len(snapshots))
			for p := range snapshots {
				snapshotPaths = append(snapshotPaths, p)
			}
			resp["message"] = fmt.Sprintf("All %d file(s) acquired. SYNC REQUIRED: %d file(s) have updated content from another agent — WRITE the content from file_snapshots to your local files BEFORE editing: %s. Then read the updated files and edit on top.",
				len(acquired), len(snapshots), strings.Join(snapshotPaths, ", "))
		} else if len(fileChanges) > 0 {
			// No snapshots, just metadata — fall back to re-read warning.
			resp["message"] = fmt.Sprintf("All %d file(s) acquired. Re-read these files before editing — they may have been modified by the previous holder.", len(acquired))
		}

		if len(fileChanges) > 0 {
			resp["file_changes"] = fileChanges
		}
	} else {
		resp["message"] = fmt.Sprintf("All %d file(s) acquired. Re-read these files before editing — they may have been modified by the previous holder.", len(acquired))
	}

	return toolJSON(resp), nil
}

// ─── Long-term project context ───────────────────────────────────────────────

func (s *Server) handleContext(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if e := getAuthError(ctx); e != "" {
		return toolError("unauthorized: " + e), nil
	}
	team := getTeam(ctx)

	tc := s.teamCtx.Get(ctx, team.TeamID)
	pc := tc.ProjectContext
	resp := map[string]any{
		"ok":           true,
		"instructions": "",
		"version":      0,
		"content":      "",
	}
	if pc != nil {
		resp["instructions"] = pc.Instructions
		resp["version"] = pc.Version
		resp["content"] = pc.Content
		if pc.Summary != "" {
			resp["summary"] = pc.Summary
		}
		if pc.UpdatedAt != "" {
			resp["updated_at"] = pc.UpdatedAt
		}
		if pc.UpdatedBy != "" {
			resp["updated_by"] = pc.UpdatedBy
		}
		if pc.UpdatedByAgent != "" {
			resp["updated_by_agent"] = pc.UpdatedByAgent
		}
	}

	if pc != nil && pc.Instructions != "" {
		resp["message"] = "Long-term project context retrieved. Follow the owner's instructions above when updating — they describe exactly what belongs here."
	} else {
		resp["message"] = "Long-term project context retrieved. No owner instructions are set for this team yet; ask a team admin to fill them in via the dashboard."
	}

	return toolJSON(resp), nil
}

func (s *Server) handleContextUpdate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if e := getAuthError(ctx); e != "" {
		return toolError("unauthorized: " + e), nil
	}
	team := getTeam(ctx)
	sessID := getSessionID(ctx)

	content, err := req.RequireString("content")
	if err != nil {
		return toolError("content is required"), nil
	}
	if len(content) > 200000 {
		return toolError("content must be 200000 characters or less"), nil
	}
	summary := getArgString(req, "summary", "")

	pc, err := s.teamCtx.UpdateProjectContext(team.TeamID, content, summary, sessID)
	if err != nil {
		log.Printf("ERROR update project context: %v", err)
		return toolError("failed to update project context"), nil
	}

	resp := map[string]any{
		"ok":           true,
		"version":      pc.Version,
		"instructions": pc.Instructions,
		"content":      pc.Content,
	}
	if pc.Instructions != "" {
		resp["message"] = fmt.Sprintf("Project context saved as v%d. Owner instructions above describe what should live here — re-read them before any future update.", pc.Version)
	} else {
		resp["message"] = fmt.Sprintf("Project context saved as v%d. No owner instructions set yet; consider asking an admin to fill them in via the dashboard.", pc.Version)
	}
	return toolJSON(resp), nil
}

// handleSwitchTeam rebinds the session's active team to the one named in
// req.team (slug or id). Only meaningful for user-scoped keys; team-scoped
// keys can technically call it but can only "switch" to their single team.
//
// Refuses if the session has an active work item — those are team-bound in
// Redis, so switching mid-work would orphan them. The agent must park or
// finish first.
func (s *Server) handleSwitchTeam(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if e := getAuthError(ctx); e != "" {
		return toolError("unauthorized: " + e), nil
	}
	team := getTeam(ctx)
	kc := getKey(ctx)
	sessID := getSessionID(ctx)
	if team == nil || kc == nil || sessID == "" {
		return toolError("unauthorized"), nil
	}

	target := strings.TrimSpace(req.GetString("team", ""))
	if target == "" {
		return toolError("team is required (slug or id)"), nil
	}

	// Find the target in the accessible list by id or slug.
	var match *auth.TeamContext
	for _, t := range kc.Teams {
		if t.TeamID == target || t.TeamSlug == target {
			match = t
			break
		}
	}
	if match == nil {
		available := make([]string, 0, len(kc.Teams))
		for _, t := range kc.Teams {
			available = append(available, t.TeamSlug)
		}
		return toolError(fmt.Sprintf("team %q is not accessible with this API key. Accessible teams: %s", target, strings.Join(available, ", "))), nil
	}

	// No-op switch is idempotent, not an error.
	if match.TeamID == team.TeamID {
		return toolJSON(map[string]any{
			"ok":        true,
			"team_id":   match.TeamID,
			"team_slug": match.TeamSlug,
			"message":   fmt.Sprintf("Already on team %s.", match.TeamSlug),
		}), nil
	}

	// Refuse if there's active work on the current team — switching would
	// orphan work/leases that are keyed under the old team in Redis.
	if w, _ := s.works.GetBySession(ctx, team.TeamID, sessID); w != nil && w.Status == "active" {
		return toolError(fmt.Sprintf("cannot switch teams while you have active work (work_id=%s, plan=%q) on team %s. Park or finish it first, then retry asynkor_switch_team.", w.ID, w.Plan, team.TeamSlug)), nil
	}

	if err := s.sessions.SetActiveTeam(ctx, sessID, match.TeamID, match.HeartbeatInterval); err != nil {
		log.Printf("switch_team: SetActiveTeam error: %v", err)
		return toolError("failed to persist team switch"), nil
	}

	return toolJSON(map[string]any{
		"ok":        true,
		"team_id":   match.TeamID,
		"team_slug": match.TeamSlug,
		"plan":      match.Plan,
		"message":   fmt.Sprintf("Active team switched to %s. The next tool call will operate on this team.", match.TeamSlug),
	}), nil
}
