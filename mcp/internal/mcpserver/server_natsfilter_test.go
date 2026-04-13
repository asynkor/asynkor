package mcpserver

import "testing"

// TestTeamIDFromSubject locks in the parsing rule used by the NATS routing
// code: subjects of the form asynkor.team.<TEAM_ID>.<event...> yield the
// team ID, and any other shape returns "" so the message is dropped instead
// of being broadcast to the wrong team.
func TestTeamIDFromSubject(t *testing.T) {
	cases := []struct {
		subj string
		want string
	}{
		{"asynkor.team.abc123.work.started", "abc123"},
		{"asynkor.team.abc123.work.completed", "abc123"},
		{"asynkor.team.abc123.lease.registered", "abc123"},
		{"asynkor.team.uuid-with-dashes-1234.work.started", "uuid-with-dashes-1234"},

		// Malformed shapes — must return "".
		{"asynkor.team.abc123", ""},          // missing event
		{"asynkor.work.abc123.started", ""},  // wrong second token
		{"team.abc123.work.started", ""},     // missing root
		{"", ""},                             // empty
		{"asynkor", ""},                      // single token
		{"asynkor.team", ""},                 // two tokens
		{"random.unrelated.subject.here", ""},
	}
	for _, c := range cases {
		got := teamIDFromSubject(c.subj)
		if got != c.want {
			t.Errorf("teamIDFromSubject(%q) = %q, want %q", c.subj, got, c.want)
		}
	}
}
