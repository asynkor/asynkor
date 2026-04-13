package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"asynkor/mcp/config"
	"asynkor/mcp/internal/activity"
	"asynkor/mcp/internal/auth"
	"asynkor/mcp/internal/lease"
	"asynkor/mcp/internal/natsbus"
	"asynkor/mcp/internal/session"
	"asynkor/mcp/internal/teamctx"
	"asynkor/mcp/internal/work"
)

type contextKey string

const (
	ctxKeyTeam     contextKey = "team_ctx"
	ctxKeySession  contextKey = "session_id"
	ctxKeyHostname contextKey = "hostname"
	ctxKeyError    contextKey = "auth_error"
)

type Server struct {
	cfg       *config.Config
	validator *auth.Validator
	sessions  *session.Store
	works     *work.Store
	leases    *lease.Store
	nats      *natsbus.Client
	activity  *activity.Store
	teamCtx   *teamctx.Store

	sessionTeams sync.Map
}

func New(
	cfg *config.Config,
	validator *auth.Validator,
	sessionStore *session.Store,
	workStore *work.Store,
	leaseStore *lease.Store,
	nats *natsbus.Client,
	activityStore *activity.Store,
	teamCtxStore *teamctx.Store,
) *Server {
	return &Server{
		cfg:       cfg,
		validator: validator,
		sessions:  sessionStore,
		works:     workStore,
		leases:    leaseStore,
		nats:      nats,
		activity:  activityStore,
		teamCtx:   teamCtxStore,
	}
}

func (s *Server) Start() error {
	hooks := &server.Hooks{}
	hooks.AddOnUnregisterSession(s.onSessionClose)

	mcpServer := server.NewMCPServer(
		"asynkor-mcp",
		"0.2.0",
		server.WithToolCapabilities(true),
		server.WithResourceCapabilities(true, true),
		server.WithHooks(hooks),
	)

	s.registerTools(mcpServer)
	s.registerResources(mcpServer)

	sseServer := server.NewSSEServer(mcpServer,
		server.WithSSEContextFunc(s.makeContextFunc()),
	)

	// Subscribe to all team work events with a wildcard, then route each
	// notification only to sessions belonging to the team in the subject.
	// Using SendNotificationToAllClients (the previous behaviour) leaked
	// timing information about every team's activity to every connected
	// agent — small payload but a real cross-tenant signal.
	cancelNats, err := s.nats.Subscribe("asynkor.team.*.work.*", func(subj string, _ []byte) {
		teamID := teamIDFromSubject(subj)
		if teamID == "" {
			log.Printf("nats: malformed subject %q — dropping notification", subj)
			return
		}
		var sent int
		s.sessionTeams.Range(func(k, v any) bool {
			if v.(string) != teamID {
				return true
			}
			sessID := k.(string)
			if err := mcpServer.SendNotificationToSpecificClient(sessID,
				"notifications/resources/updated",
				map[string]any{"uri": "asynkor://briefing"},
			); err != nil {
				// Session may have just closed; the next NATS event will
				// skip it because onSessionClose drains the map.
				log.Printf("nats: notify session %s failed: %v", sessID, err)
			} else {
				sent++
			}
			return true
		})
		log.Printf("nats: %s → %d session(s) on team %s", subj, sent, teamID)
	})
	if err != nil {
		log.Printf("nats: failed to subscribe to work events: %v (push updates disabled)", err)
	} else {
		defer cancelNats()
	}

	log.Printf("asynkor-mcp listening on :%s", s.cfg.Port)
	return sseServer.Start(":" + s.cfg.Port)
}

func (s *Server) makeContextFunc() server.SSEContextFunc {
	return func(ctx context.Context, r *http.Request) context.Context {
		apiKey := extractAPIKey(r)
		if apiKey == "" {
			return context.WithValue(ctx, ctxKeyError, "missing Authorization header or api_key param")
		}

		teamCtx, err := s.validator.Validate(apiKey)
		if err != nil {
			log.Printf("auth: invalid key: %v", err)
			return context.WithValue(ctx, ctxKeyError, "invalid_key")
		}

		// Use the stable SSE session ID assigned by mark3labs/mcp-go in
		// handleSSE and echoed back on every POST as ?sessionId=...  Using a
		// fresh UUID per POST (the previous behaviour) broke work lookup
		// across tool calls: asynkor_start stored session→work under one ID
		// and asynkor_finish searched under a different one, so finish
		// always returned "no active work found". It also flooded Redis
		// with a new session row per tool call.
		sessID := r.URL.Query().Get("sessionId")
		if sessID == "" {
			return context.WithValue(ctx, ctxKeyError, "missing sessionId query param")
		}

		hostname := sanitizeHeader(r.Header.Get("X-Hostname"), 255)
		agent := sanitizeHeader(r.Header.Get("X-Agent"), 255)
		agentVersion := sanitizeHeader(r.Header.Get("X-Agent-Version"), 255)

		// Upsert the session row. Called on every POST, but with a stable
		// sessID this is a cheap idempotent refresh: SET overwrites the JSON
		// (also refreshing TTL) and SADD is a no-op for existing members.
		sess := &session.Session{
			ID:           sessID,
			TeamID:       teamCtx.TeamID,
			Agent:        agent,
			AgentVersion: agentVersion,
			Hostname:     hostname,
			ConnectedAt:  time.Now().UTC(),
			LastSeenAt:   time.Now().UTC(),
		}
		if err := s.sessions.Create(ctx, sess, teamCtx.HeartbeatInterval); err != nil {
			log.Printf("session create error: %v", err)
		}

		// Record the connect event and remember the sessID→teamID mapping
		// exactly once per connection. The mapping is drained by
		// onSessionClose when the SSE stream actually closes, so we get a
		// single disconnect event (not one per POST).
		if _, alreadySeen := s.sessionTeams.LoadOrStore(sessID, teamCtx.TeamID); !alreadySeen {
			s.activity.RecordEvent(ctx, teamCtx.TeamID, activity.Event{
				Type:      "session.connected",
				SessionID: sessID,
				Agent:     sess.Agent,
				Hostname:  sess.Hostname,
			})
			log.Printf("agent connected: team=%s agent=%s host=%s session=%s",
				teamCtx.TeamSlug, sess.Agent, sess.Hostname, sessID)
		}

		ctx = context.WithValue(ctx, ctxKeyTeam, teamCtx)
		ctx = context.WithValue(ctx, ctxKeySession, sessID)
		ctx = context.WithValue(ctx, ctxKeyHostname, hostname)
		return ctx
	}
}

// onSessionClose is invoked by mark3labs/mcp-go when an SSE connection's
// lifetime ends (client disconnect, server shutdown, or handler return).
// It cleans up the Redis session row and emits a single disconnect event.
//
// The incoming ctx is the SSE request's context, which is already cancelled
// by the time the hook fires — that's why we switch to a background context
// for the cleanup operations.
func (s *Server) onSessionClose(_ context.Context, clientSession server.ClientSession) {
	sessID := clientSession.SessionID()
	v, loaded := s.sessionTeams.LoadAndDelete(sessID)
	if !loaded {
		// This SSE connection never processed a POST (e.g. auth failed or
		// client disconnected before sending a message), so nothing to clean.
		return
	}
	teamID := v.(string)

	bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := s.sessions.Delete(bgCtx, teamID, sessID); err != nil {
		log.Printf("session delete on close error: %v", err)
	}

	// Release leases and auto-park active work so it doesn't pollute the
	// active set and trigger false overlap warnings for future agents.
	if w, _ := s.works.GetBySession(bgCtx, teamID, sessID); w != nil {
		if err := s.leases.ReleaseByWork(bgCtx, teamID, w.ID); err != nil {
			log.Printf("lease release on close error: %v", err)
		}
		if w.Status == "active" {
			if _, err := s.works.Park(bgCtx, teamID, w.ID, "", "auto-parked: session disconnected"); err != nil {
				log.Printf("auto-park on close error: %v", err)
			} else {
				log.Printf("auto-parked work %s on session disconnect", w.ID)
			}
		}
	}
	s.activity.RecordEvent(bgCtx, teamID, activity.Event{
		Type:      "session.disconnected",
		SessionID: sessID,
	})
	log.Printf("agent disconnected: team=%s session=%s", teamID, sessID)
}

func (s *Server) registerTools(mcpServer *server.MCPServer) {
	mcpServer.AddTool(
		mcp.NewTool("asynkor_briefing",
			mcp.WithDescription("Get the current team state: who is working on what, what was recently completed, and what follow-up tasks are waiting. Call this at the beginning of each work session to orient yourself before starting."),
		),
		s.handleBriefing,
	)

	mcpServer.AddTool(
		mcp.NewTool("asynkor_start",
			mcp.WithDescription("Declare the start of your work session. Describe in plain language what you are about to do. The server will check whether any teammate is already working on the same files or a similar plan and return warnings (or block the start, depending on the team's conflict_mode). If you are picking up an open follow-up from the briefing, pass its followup_id."),
			mcp.WithString("plan",
				mcp.Description("What you are going to do, in your own words. E.g. 'Adding refresh token rotation to the auth module'"),
				mcp.Required(),
			),
			mcp.WithString("paths",
				mcp.Description("Optional comma-separated list of files you expect to touch, e.g. 'src/auth/jwt.ts,src/auth/middleware.ts'. Used for path-level overlap detection against other active work. Highly recommended when you know the target files — plan-text similarity is the fallback when paths are absent."),
			),
			mcp.WithString("followup_id",
				mcp.Description("ID of the follow-up you are picking up (from asynkor_briefing output). Marks it as taken so teammates don't double-pick. If another agent already claimed it, the call returns an error with the claimant's work_id."),
			),
			mcp.WithString("handoff_id",
				mcp.Description("ID of a parked work session to resume (from asynkor_briefing output). Inherits the parked work's plan, progress, decisions, and learnings so you can continue where the previous agent left off."),
			),
			mcp.WithString("acknowledge_overlap",
				mcp.Description("Comma-separated list of work_ids from a prior overlap warning. Only meaningful in block mode: if the initial asynkor_start was refused with an 'overlap' error, surface the conflicts to the user, coordinate, and retry with this field populated with every conflicting work_id to proceed deliberately."),
			),
			mcp.WithString("acknowledge_zone",
				mcp.Description("Comma-separated list of zone IDs from a prior zone block. Required when planned paths fall inside a zone whose action is 'confirm' or 'block': surface the zone restrictions to the user, get explicit go-ahead, and retry with this field populated with every triggering zone_id."),
			),
		),
		s.handleStart,
	)

	mcpServer.AddTool(
		mcp.NewTool("asynkor_check",
			mcp.WithDescription("Check team rules, zones, relevant memories, active work, and recent history for specific file paths before modifying them. Returns applicable rules, zone restrictions, historical context, and warnings about those paths."),
			mcp.WithString("paths",
				mcp.Description("Comma-separated file paths to check, e.g. 'src/auth/middleware.ts,src/auth/jwt.ts'"),
				mcp.Required(),
			),
			mcp.WithReadOnlyHintAnnotation(true),
		),
		s.handleCheck,
	)

	mcpServer.AddTool(
		mcp.NewTool("asynkor_remember",
			mcp.WithDescription("Save a piece of knowledge to the team's persistent codebase memory. Use this to record important context that future agents should know: gotchas, architectural decisions, incident notes, file-specific warnings."),
			mcp.WithString("content",
				mcp.Description("The knowledge to save. Be specific and actionable."),
				mcp.Required(),
			),
			mcp.WithString("paths",
				mcp.Description("Comma-separated file paths or glob patterns this memory relates to, e.g. 'src/auth/**,src/middleware.ts'"),
			),
			mcp.WithString("tags",
				mcp.Description("Comma-separated tags, e.g. 'incident,auth,critical'"),
			),
		),
		s.handleRemember,
	)

	mcpServer.AddTool(
		mcp.NewTool("asynkor_finish",
			mcp.WithDescription("Declare the completion of your work. Summarise what you actually did and list any follow-up actions that teammates should know about. Follow-ups can be anything: a task to finish, something to review, a conversation to have, something to monitor — anything connected to this work."),
			mcp.WithString("work_id",
				mcp.Description("Your work ID from asynkor_start. Only needed if the session reconnected and the server can't find your work by session — pass the work_id you received from asynkor_start as a fallback."),
			),
			mcp.WithString("result",
				mcp.Description("What you did and what changed. Be specific: which files were modified, what behaviour changed, what is still broken or incomplete."),
				mcp.Required(),
			),
			mcp.WithString("learnings",
				mcp.Description("Key things learned during this work that future agents should know"),
			),
			mcp.WithString("decisions",
				mcp.Description("Architectural or design decisions made and why"),
			),
			mcp.WithString("files_touched",
				mcp.Description("Comma-separated list of file paths modified during this work"),
			),
			mcp.WithString("file_snapshots",
				mcp.Description("JSON object mapping file paths to their current content, e.g. {\"src/api.ts\": \"file content...\"}. Read each modified file and include its content here so agents on other machines get the updated version when they acquire the lease. Critical for cross-machine coordination."),
			),
			mcp.WithString("followups",
				mcp.Description(`Optional JSON array of follow-up actions. Each item: {"description":"what needs to happen","area":"optional path or area","priority":"critical|normal|low","type":"free text, e.g. review, task, discuss, watch, docs","context":"optional background context","tried":"optional what was already tried","watch_out":"optional warnings or gotchas"}`),
			),
		),
		s.handleFinish,
	)

	mcpServer.AddTool(
		mcp.NewTool("asynkor_park",
			mcp.WithDescription("Park your current work for later resumption. Use this when the work is not done but you need to stop — another agent or developer can pick it up later via asynkor_start with handoff_id. Leases are released so files are not blocked. The short-term context (plan, progress, decisions) is saved as a handoff."),
			mcp.WithString("work_id",
				mcp.Description("Your work ID from asynkor_start. Only needed if the session reconnected and the server can't find your work by session."),
			),
			mcp.WithString("progress",
				mcp.Description("What's done and what's left. Be specific so the next agent can pick up without guessing."),
				mcp.Required(),
			),
			mcp.WithString("notes",
				mcp.Description("Developer notes for whoever picks this up — blockers, dependencies, things to watch out for."),
			),
			mcp.WithString("files_touched",
				mcp.Description("Comma-separated list of files modified so far."),
			),
			mcp.WithString("learnings",
				mcp.Description("Key things learned during this work that future agents should know."),
			),
			mcp.WithString("decisions",
				mcp.Description("Architectural or design decisions made and why."),
			),
		),
		s.handlePark,
	)

	mcpServer.AddTool(
		mcp.NewTool("asynkor_cancel",
			mcp.WithDescription("Cancel a stale or orphaned work item. Removes it from the active set and releases its leases. Use this to clean up work items from disconnected sessions that are blocking overlap detection or holding stale leases. Requires the work_id from the briefing output."),
			mcp.WithString("work_id",
				mcp.Description("The work ID to cancel (from asynkor_briefing output)."),
				mcp.Required(),
			),
		),
		s.handleCancel,
	)

	mcpServer.AddTool(
		mcp.NewTool("asynkor_lease_acquire",
			mcp.WithDescription("Acquire leases on file paths during active work. Other agents trying to edit these files will be told to wait. Leases auto-expire after 5 minutes and are refreshed automatically. Call this before editing files that weren't in your original asynkor_start paths."),
			mcp.WithString("paths",
				mcp.Description("Comma-separated file paths to lease, e.g. 'src/auth/jwt.ts,src/auth/middleware.ts'"),
				mcp.Required(),
			),
		),
		s.handleLeaseAcquire,
	)

	mcpServer.AddTool(
		mcp.NewTool("asynkor_lease_wait",
			mcp.WithDescription("Try to acquire leased files, waiting up to 25-30 seconds. If the files are freed within that window, returns them acquired. If still blocked, returns status: still_blocked — work on other files first, then retry. After acquiring, you MUST re-read the files before editing — they may have been changed by the previous holder."),
			mcp.WithString("paths",
				mcp.Description("Comma-separated file paths to wait for and acquire."),
				mcp.Required(),
			),
			mcp.WithString("timeout_seconds",
				mcp.Description("Maximum seconds to wait. Default 25, max 30. Capped to fit MCP transport timeout."),
			),
		),
		s.handleLeaseWait,
	)
}

func (s *Server) registerResources(mcpServer *server.MCPServer) {
	resource := mcp.NewResource(
		"asynkor://briefing",
		"Team briefing",
		mcp.WithResourceDescription("Live team state: active work, recent completions, and open follow-up tasks. Subscribe for real-time updates when teammates start or finish work."),
		mcp.WithMIMEType("application/json"),
	)
	mcpServer.AddResource(resource, s.handleBriefingResource)
}

func (s *Server) handleBriefingResource(ctx context.Context, _ mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	if e := getAuthError(ctx); e != "" {
		return nil, fmt.Errorf("unauthorized: %s", e)
	}
	team := getTeam(ctx)
	if team == nil {
		return nil, fmt.Errorf("unauthorized")
	}

	active, _ := s.works.ListActive(ctx, team.TeamID)
	recent, _ := s.works.ListRecent(ctx, team.TeamID, 5)

	data, _ := json.Marshal(map[string]any{
		"team":   team.TeamSlug,
		"active": active,
		"recent": recent,
	})

	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      "asynkor://briefing",
			MIMEType: "application/json",
			Text:     string(data),
		},
	}, nil
}

func extractAPIKey(r *http.Request) string {
	authHeader := r.Header.Get("Authorization")
	if strings.HasPrefix(authHeader, "Bearer ") {
		return strings.TrimPrefix(authHeader, "Bearer ")
	}
	// Do not fall back to query parameters. API keys in URLs get logged by
	// proxies, CDNs, and browser history, making them easy to leak.
	return ""
}

func getTeam(ctx context.Context) *auth.TeamContext {
	v, _ := ctx.Value(ctxKeyTeam).(*auth.TeamContext)
	return v
}

func getSessionID(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeySession).(string)
	return v
}

func getHostname(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyHostname).(string)
	return v
}

func getAuthError(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyError).(string)
	return v
}

// teamIDFromSubject extracts the team ID from a NATS subject of the form
// "asynkor.team.<TEAM_ID>.<EVENT_PARTS...>". Returns "" if the subject is
// not in the expected shape.
func teamIDFromSubject(subj string) string {
	parts := strings.Split(subj, ".")
	if len(parts) < 4 || parts[0] != "asynkor" || parts[1] != "team" {
		return ""
	}
	return parts[2]
}

// sanitizeHeader truncates a header value and strips control characters
// to prevent log injection and other attacks via agent-supplied headers.
func sanitizeHeader(s string, maxLen int) string {
	if len(s) > maxLen {
		s = s[:maxLen]
	}
	return strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, s)
}
