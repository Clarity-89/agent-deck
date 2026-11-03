package main

import (
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A fake that ignores SIGHUP must still exit when its pane closes.
func TestClaudeLikeFakeExitsWhenItsPaneIsGone(t *testing.T) {
	sess, transcript := claudeLikeFakeControl(t, "idle", map[string]any{claudeLikeFakeIgnoresHangup: true})
	script := filepath.Join(filepath.Dir(transcript), "claude")
	t.Cleanup(func() {
		for _, raw := range liveProcessesRunning(t, script) {
			if pid, err := strconv.Atoi(raw); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	if len(liveProcessesRunning(t, script)) == 0 {
		t.Fatalf("the Claude-like fake (%s) is not running", script)
	}
	// Kill the pane without agent-deck's process escalation masking EOF handling.
	if out, err := exec.Command("tmux", "kill-session", "-t", sess.TmuxName()).CombinedOutput(); err != nil {
		t.Fatalf("kill-session: %v\n%s", err, out)
	}
	deadline := time.Now().Add(5 * time.Second)
	for pids := liveProcessesRunning(t, script); len(pids) > 0; pids = liveProcessesRunning(t, script) {
		if time.Now().After(deadline) {
			t.Fatalf("the Claude-like fake (%s) outlived its pane: pids %v", script, pids)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Issue #2921: TestGuardedSendLeavesBusyOperatorDraftAlone failed on its
// TempDir cleanup ("directory not empty"). The Claude-like fake rewrites
// <transcript>.input-state.json through a .tmp file every 50ms, and the
// fixture's cleanup stopped it with the asynchronous tmux Kill, which returns
// as soon as kill-session does, before the fake has exited. TempDir's
// RemoveAll, which runs next, then raced the fake's last write.
//
// The fixture must not hand its directory back while the fake can still write
// into it: once the fixture's cleanup has run, the fake's process is gone. The
// fake here ignores SIGHUP, as Claude Code 2.1.27+ does, which turns the
// scheduling race into a certain one: it outlives kill-session.
func TestIssue2921ClaudeLikeFakeIsGoneWhenItsCleanupReturns(t *testing.T) {
	var script string
	t.Run("fixture", func(t *testing.T) {
		_, transcript := claudeLikeFakeControl(t, "busy", map[string]any{claudeLikeFakeIgnoresHangup: true})
		script = filepath.Join(filepath.Dir(transcript), "claude")
	})
	if script == "" {
		t.Fatal("fixture did not start")
	}
	if pids := liveProcessesRunning(t, script); len(pids) > 0 {
		t.Fatalf("the Claude-like fake (%s) was still running after its fixture's cleanup returned: pids %v", script, pids)
	}
}

// A zombie cannot write into a directory whose fixture is being removed.
func liveProcessesRunning(t *testing.T, path string) []string {
	t.Helper()
	out, err := exec.Command("/bin/ps", "-axww", "-o", "pid=,stat=,command=").Output()
	if err != nil {
		t.Fatalf("list processes: %v", err)
	}
	var pids []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && !strings.HasPrefix(fields[1], "Z") && slices.Contains(fields[2:], path) {
			pids = append(pids, fields[0])
		}
	}
	return pids
}
