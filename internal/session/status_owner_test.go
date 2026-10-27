package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A client warns that nothing publishes status when StatusOwner says none, so
// it relies on the owner order and on a stale TUI heartbeat no longer counting.
func TestStatusOwnerFollowsTheLiveWriter(t *testing.T) {
	inboxTestHome(t)
	const profile = "_test-status-owner"
	want := func(step, expected string) {
		t.Helper()
		got, err := StatusOwner(profile)
		if err != nil || got != expected {
			t.Fatalf("%s: StatusOwner = %q, %v; want %q", step, got, err, expected)
		}
	}

	want("no store, no daemon", StatusOwnerNone)
	dir, err := GetProfileDir(profile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.db")); !os.IsNotExist(err) {
		t.Fatalf("asking created a store: %v", err)
	}

	storage, err := NewStorageWithProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	db := storage.GetDB()

	WriteNotifyHeartbeat()
	want("daemon heartbeat, no TUI", StatusOwnerNotifyDaemon)

	if err := db.RegisterInstance(false); err != nil {
		t.Fatal(err)
	}
	want("live TUI beside the daemon", StatusOwnerTUI)

	if _, err := db.DB().Exec("UPDATE instance_heartbeats SET heartbeat = ?", time.Now().Add(-time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	want("TUI heartbeat a minute old", StatusOwnerNotifyDaemon)
}
