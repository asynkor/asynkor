package auth

type TeamContext struct {
	TeamID            string
	TeamSlug          string
	Plan              string
	HeartbeatInterval int
	ConflictMode      string
	IgnorePatterns    []string
}
