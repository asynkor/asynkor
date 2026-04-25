package auth

// TeamContext is a single team's resolved validation state. For team-scoped
// keys, the Validator returns exactly one. For user-scoped keys it returns
// one per accessible team (see KeyContext.Teams).
type TeamContext struct {
	TeamID            string
	TeamSlug          string
	Plan              string
	HeartbeatInterval int
	ConflictMode      string
	IgnorePatterns    []string
}

// KeyContext is the full result of validating an API key. For team-scoped
// keys Teams has length 1 and DefaultTeamID == Teams[0].TeamID. For user-
// scoped keys Teams lists every team the user can currently access and
// DefaultTeamID is the team Go should bind the session to on first use
// (overridden later by asynkor_switch_team).
type KeyContext struct {
	Scope          string // "team" or "user"
	UserID         string // empty for team-scoped
	DefaultTeamID  string
	Teams          []*TeamContext
}

// FindTeam returns the TeamContext matching id, or nil.
func (k *KeyContext) FindTeam(teamID string) *TeamContext {
	for _, t := range k.Teams {
		if t.TeamID == teamID {
			return t
		}
	}
	return nil
}
