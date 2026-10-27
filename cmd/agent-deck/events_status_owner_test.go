package main

import (
	"encoding/json"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// `events stats --json` reports status_owner from the -p profile's own
// state.db: a TUI heartbeat in one profile must not make another one look
// owned, or a client would skip its "nothing publishes status" warning.
func TestEventsStatsStatusOwnerFollowsTheProfile(t *testing.T) {
	parallelSubprocessTest(t)
	home := t.TempDir()
	addProfileTestSession(t, home, "work", "owner work")
	addProfileTestSession(t, home, "personal", "owner personal")

	db, err := statedb.Open(profileStateDB(t, home, "work"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.RegisterInstance(false); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ profile, want string }{{"work", "tui"}, {"personal", "none"}} {
		stdout, stderr, code := runAgentDeck(t, home, "-p", tc.profile, "events", "stats", "--json")
		var stats struct {
			StatusOwner string `json:"status_owner"`
		}
		if code != 0 || json.Unmarshal([]byte(stdout), &stats) != nil || stats.StatusOwner != tc.want {
			t.Fatalf("-p %s: status_owner %q, want %q (exit %d)\n%s%s", tc.profile, stats.StatusOwner, tc.want, code, stdout, stderr)
		}
	}
}
