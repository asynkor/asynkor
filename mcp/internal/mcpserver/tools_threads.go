package mcpserver

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// asynkor_inspect — read the live state of one work (active or parked).
// Returns plan, paths, files_touched, learnings, decisions, parked notes,
// and the leases this work currently holds. Read-only.
func (s *Server) handleInspect(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if e := getAuthError(ctx); e != "" {
		return toolError("unauthorized: " + e), nil
	}
	team := getTeam(ctx)

	workID, err := req.RequireString("work_id")
	if err != nil {
		return toolError("work_id is required (find it in asynkor_briefing under 'Active work' or as 'handoff_id' under 'Parked work')"), nil
	}
	workID = strings.TrimSpace(workID)
	if !isValidUUID(workID) {
		return toolError("work_id must be a UUID"), nil
	}

	w, err := s.works.GetByID(ctx, team.TeamID, workID)
	if err != nil {
		log.Printf("ERROR inspect work %s: %v", workID, err)
		return toolError(fmt.Sprintf("failed to inspect work: %v", err)), nil
	}
	if w == nil {
		return toolError(fmt.Sprintf("work %s not found in active or parked state — it may already be completed (check 'Recently completed' in the briefing) or cancelled", workID)), nil
	}

	// Leases held by this work — useful for "what files are they sitting on".
	allLeases, _ := s.leases.ListAll(ctx, team.TeamID)
	var heldByThis []map[string]any
	for _, l := range allLeases {
		if l.WorkID == workID {
			heldByThis = append(heldByThis, map[string]any{
				"path":          l.Path,
				"hostname":      l.Hostname,
				"plan":          l.Plan,
				"acquired_at":   l.AcquiredAt.Format(time.RFC3339),
				"acquired_ago":  time.Since(l.AcquiredAt).Round(time.Second).String(),
			})
		}
	}

	out := map[string]any{
		"id":            w.ID,
		"hostname":      w.Hostname,
		"status":        w.Status,
		"plan":          w.Plan,
		"planned_paths": w.PlannedPaths,
		"files_touched": w.FilesTouched,
		"created_at":    w.CreatedAt.Format(time.RFC3339),
		"created_ago":   time.Since(w.CreatedAt).Round(time.Second).String(),
		"leases_held":   heldByThis,
	}
	if w.Result != "" {
		out["result"] = w.Result
	}
	if w.Learnings != "" {
		out["learnings"] = w.Learnings
	}
	if w.Decisions != "" {
		out["decisions"] = w.Decisions
	}
	if w.Progress != "" {
		out["progress"] = w.Progress
	}
	if w.ParkedNotes != "" {
		out["parked_notes"] = w.ParkedNotes
	}
	if w.HandoffFrom != "" {
		out["handoff_from"] = w.HandoffFrom
	}
	if w.ParkedAt != nil {
		out["parked_at"] = w.ParkedAt.Format(time.RFC3339)
	}
	if w.CompletedAt != nil {
		out["completed_at"] = w.CompletedAt.Format(time.RFC3339)
	}
	if len(w.Followups) > 0 {
		out["followups"] = w.Followups
	}

	return toolJSON(out), nil
}

// asynkor_ask — open a new thread targeting another work, host, or the team.
// First message is the question. The thread shows up in the target's inbox
// (and the dashboard); they reply via asynkor_reply.
func (s *Server) handleAsk(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if e := getAuthError(ctx); e != "" {
		return toolError("unauthorized: " + e), nil
	}
	team := getTeam(ctx)
	hostname := getHostname(ctx)
	sessID := getSessionID(ctx)

	// Best-effort: resolve the asker's current work_id by session.
	var openerWorkID string
	if w, _ := s.works.GetBySession(ctx, team.TeamID, sessID); w != nil {
		openerWorkID = w.ID
	}

	target, err := req.RequireString("target")
	if err != nil {
		return toolError("target is required: 'work:<work_id>' (specific session), 'host:<hostname>' (a developer), or 'team' (broadcast — anyone replies)"), nil
	}
	target = strings.TrimSpace(target)

	var targetKind, targetValue string
	switch {
	case target == "team":
		targetKind = "team"
		targetValue = "team"
	case strings.HasPrefix(target, "work:"):
		targetKind = "work"
		targetValue = strings.TrimSpace(strings.TrimPrefix(target, "work:"))
		if !isValidUUID(targetValue) {
			return toolError("target work_id must be a UUID — copy it from asynkor_briefing under 'Active work' or 'Parked work'"), nil
		}
	case strings.HasPrefix(target, "host:"):
		targetKind = "host"
		targetValue = strings.TrimSpace(strings.TrimPrefix(target, "host:"))
		if targetValue == "" {
			return toolError("target hostname is empty — pass 'host:<hostname>'"), nil
		}
	default:
		return toolError("target must be 'work:<work_id>', 'host:<hostname>', or 'team' (got '" + target + "')"), nil
	}

	if targetKind == "host" && targetValue == hostname {
		return toolError("can't address a thread to your own host — talk to yourself in your own head, not via Asynkor"), nil
	}
	if targetKind == "work" && targetValue == openerWorkID {
		return toolError("can't address a thread to your own work_id"), nil
	}

	topic, err := req.RequireString("topic")
	if err != nil {
		return toolError("topic is required — a short subject line (e.g. 'JWT rotation strategy')"), nil
	}
	topic = strings.TrimSpace(topic)
	if len(topic) > 200 {
		return toolError("topic must be 200 characters or less"), nil
	}

	body, err := req.RequireString("question")
	if err != nil {
		return toolError("question is required — the first message body for the thread"), nil
	}
	body = strings.TrimSpace(body)
	if len(body) > 8000 {
		return toolError("question must be 8000 characters or less"), nil
	}

	pathsRaw := getArgString(req, "context_paths", "")
	var paths []string
	if pathsRaw != "" {
		for _, p := range strings.Split(pathsRaw, ",") {
			if t := strings.TrimSpace(p); t != "" {
				paths = append(paths, filepath.Clean(t))
			}
		}
	}

	thread, err := s.teamCtx.OpenThread(team.TeamID, openerWorkID, hostname, targetKind, targetValue, topic, body, paths)
	if err != nil {
		log.Printf("ERROR open thread: %v", err)
		return toolError(fmt.Sprintf("failed to open thread: %v", err)), nil
	}

	return toolJSON(map[string]any{
		"ok":         true,
		"thread_id":  thread.ID,
		"status":     thread.Status,
		"target":     fmt.Sprintf("%s:%s", thread.TargetKind, thread.TargetValue),
		"message":    "Thread opened. The target sees it in their inbox (asynkor_inbox) and the next briefing. Carry on with other work — check back via asynkor_thread when you need the answer.",
	}), nil
}

// asynkor_inbox — list open threads addressed to me (this work_id, this
// host, or broadcast to the team). Same data the briefing surfaces in
// summary form.
func (s *Server) handleInbox(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if e := getAuthError(ctx); e != "" {
		return toolError("unauthorized: " + e), nil
	}
	team := getTeam(ctx)
	hostname := getHostname(ctx)
	sessID := getSessionID(ctx)

	var workID string
	if w, _ := s.works.GetBySession(ctx, team.TeamID, sessID); w != nil {
		workID = w.ID
	}

	threads, err := s.teamCtx.ThreadInbox(team.TeamID, workID, hostname)
	if err != nil {
		log.Printf("ERROR thread inbox: %v", err)
		return toolError(fmt.Sprintf("failed to load inbox: %v", err)), nil
	}

	items := make([]map[string]any, 0, len(threads))
	for _, t := range threads {
		items = append(items, map[string]any{
			"thread_id":   t.ID,
			"topic":       t.Topic,
			"status":      t.Status,
			"target":      fmt.Sprintf("%s:%s", t.TargetKind, t.TargetValue),
			"opener_host": t.OpenerHost,
			"updated_at":  t.UpdatedAt,
			"updated_ago": relativeTime(t.UpdatedAt),
		})
	}

	return toolJSON(map[string]any{
		"count":   len(items),
		"threads": items,
		"hint":    "Use asynkor_thread(thread_id) for the full transcript and asynkor_reply(thread_id, body) to respond. Closing a thread when the question is fully answered keeps the team inbox tidy.",
	}), nil
}

// asynkor_thread — read the full transcript of one thread.
func (s *Server) handleThread(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if e := getAuthError(ctx); e != "" {
		return toolError("unauthorized: " + e), nil
	}
	team := getTeam(ctx)

	threadID, err := req.RequireString("thread_id")
	if err != nil {
		return toolError("thread_id is required — copy it from asynkor_inbox or the briefing"), nil
	}
	threadID = strings.TrimSpace(threadID)
	if !isValidUUID(threadID) {
		return toolError("thread_id must be a UUID"), nil
	}

	twm, err := s.teamCtx.GetThread(team.TeamID, threadID)
	if err != nil {
		log.Printf("ERROR get thread %s: %v", threadID, err)
		return toolError(fmt.Sprintf("failed to load thread: %v", err)), nil
	}

	msgs := make([]map[string]any, 0, len(twm.Messages))
	for _, m := range twm.Messages {
		msgs = append(msgs, map[string]any{
			"author_host":    m.AuthorHost,
			"author_work_id": m.AuthorWorkID,
			"body":           m.Body,
			"created_at":     m.CreatedAt,
			"created_ago":    relativeTime(m.CreatedAt),
		})
	}

	return toolJSON(map[string]any{
		"thread_id":     twm.Thread.ID,
		"topic":         twm.Thread.Topic,
		"status":        twm.Thread.Status,
		"target":        fmt.Sprintf("%s:%s", twm.Thread.TargetKind, twm.Thread.TargetValue),
		"opener_host":   twm.Thread.OpenerHost,
		"context_paths": twm.Thread.ContextPaths,
		"created_at":    twm.Thread.CreatedAt,
		"messages":      msgs,
	}), nil
}

// asynkor_reply — append a message to a thread. Optionally close it when
// the question is fully answered.
func (s *Server) handleReply(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if e := getAuthError(ctx); e != "" {
		return toolError("unauthorized: " + e), nil
	}
	team := getTeam(ctx)
	hostname := getHostname(ctx)
	sessID := getSessionID(ctx)

	var workID string
	if w, _ := s.works.GetBySession(ctx, team.TeamID, sessID); w != nil {
		workID = w.ID
	}

	threadID, err := req.RequireString("thread_id")
	if err != nil {
		return toolError("thread_id is required — copy it from asynkor_inbox or the briefing"), nil
	}
	threadID = strings.TrimSpace(threadID)
	if !isValidUUID(threadID) {
		return toolError("thread_id must be a UUID"), nil
	}

	body, err := req.RequireString("body")
	if err != nil {
		return toolError("body is required — your message"), nil
	}
	body = strings.TrimSpace(body)
	if len(body) == 0 {
		return toolError("body cannot be empty"), nil
	}
	if len(body) > 8000 {
		return toolError("body must be 8000 characters or less"), nil
	}

	closeStr := getArgString(req, "close", "")
	close := closeStr == "true" || closeStr == "1"

	msg, err := s.teamCtx.ReplyThread(team.TeamID, threadID, workID, hostname, body, close)
	if err != nil {
		log.Printf("ERROR reply thread %s: %v", threadID, err)
		return toolError(fmt.Sprintf("failed to reply: %v", err)), nil
	}

	out := map[string]any{
		"ok":         true,
		"message_id": msg.ID,
		"created_at": msg.CreatedAt,
	}
	if close {
		out["status"] = "closed"
		out["message"] = "Reply posted and thread closed. If the answer included a durable decision, consider asynkor_context_update to merge it into the long-term project context."
	} else {
		out["message"] = "Reply posted. The other side sees it in their next inbox/briefing."
	}
	return toolJSON(out), nil
}
