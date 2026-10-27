package session

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// The owner of a profile's statuses is the process that writes their
// transitions to state.db; with [macapp] status_events on, that write is what
// publishes session.status frames (docs/status-detection.md, "Who computes").
const (
	StatusOwnerTUI          = "tui"
	StatusOwnerNotifyDaemon = "notify-daemon"
	StatusOwnerNone         = "none"
)

// StatusOwner reports who owns status transitions for profile: a TUI (or
// `web --no-tui`, which registers the same heartbeat) with a fresh heartbeat,
// else a notify daemon whose heartbeat is fresh (ReadWriterStatus), however it
// was started, else nobody. It opens state.db read-only and creates nothing,
// so a profile without a store keeps having none.
func StatusOwner(profile string) (string, error) {
	dir, err := GetProfileDir(profile)
	if err != nil {
		return "", fmt.Errorf("resolving directory of profile %q: %w", profile, err)
	}
	dbPath := filepath.Join(dir, storeRootStateDBName)
	if _, err := os.Stat(dbPath); err == nil {
		db, err := statedb.OpenReadOnlyLive(dbPath)
		if err != nil {
			return "", fmt.Errorf("opening %s read-only: %w", dbPath, err)
		}
		defer func() { _ = db.Close() }()
		if tuiOwnsStatus(db) {
			return StatusOwnerTUI, nil
		}
	}
	if ReadWriterStatus().Running {
		return StatusOwnerNotifyDaemon, nil
	}
	return StatusOwnerNone, nil
}

// tuiOwnsStatus reports whether db holds a fresh TUI heartbeat. While it does,
// that TUI writes the statuses and the notify daemon only reads them.
func tuiOwnsStatus(db *statedb.StateDB) bool {
	n, err := db.AliveInstanceCount()
	return err == nil && n > 0
}
