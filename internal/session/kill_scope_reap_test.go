// Regression test for orphaned daemonized children surviving session
// deletion (observed as nx run-executor processes burning 100% CPU for
// days after their agent-deck session was deleted).
//
// killInternal must read the per-launch AGENTDECK_KILL_SCOPE marker
// from the live tmux session and, after tmux teardown, sweep any
// process still carrying it — including processes that daemonized out
// of the pane tree and are therefore invisible to pane-walk reaping.
package session

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// TestHelperKillScope re-execs as the daemonizing pane child: it spawns
// a grandchild in its own session (inheriting the pane environment,
// marker included) and exits, leaving the grandchild parented to PID 1.
func TestHelperKillScope(t *testing.T) {
	if os.Getenv("KILL_SCOPE_HELPER_MODE") == "" {
		t.Skip("helper entrypoint, not a test")
	}
	switch os.Getenv("KILL_SCOPE_HELPER_MODE") {
	case "sleep":
		time.Sleep(120 * time.Second)
	case "orphan":
		exe, err := os.Executable()
		if err != nil {
			fmt.Println("ERR:", err)
			return
		}
		var env []string
		for _, kv := range os.Environ() {
			if strings.HasPrefix(kv, "KILL_SCOPE_HELPER_MODE=") {
				continue
			}
			env = append(env, kv)
		}
		cmd := exec.Command(exe, "-test.run=^TestHelperKillScope$", "-test.timeout=140s")
		cmd.Env = append(env, "KILL_SCOPE_HELPER_MODE=sleep")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			fmt.Println("ERR:", err)
			return
		}
		fmt.Println("GRANDCHILD:", cmd.Process.Pid)
		_ = cmd.Process.Release()
	}
}

func TestKillAndWait_SweepsDaemonizedPaneChild(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("unix-only")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	pidFile := t.TempDir() + "/grandchild.pid"
	paneCmd := fmt.Sprintf("KILL_SCOPE_HELPER_MODE=orphan %s -test.run='^TestHelperKillScope$' -test.v > %s; sleep 60",
		exe, pidFile)

	sess := tmux.NewSession("agent-deck-killscope-instance-e2e", t.TempDir())
	sess.RunCommandAsInitialProcess = true
	if err := sess.Start(paneCmd); err != nil {
		t.Skipf("could not start tmux session in this environment: %v", err)
	}
	t.Cleanup(func() { _ = sess.KillAndWait() })

	if scope, err := sess.GetEnvironment(tmux.KillScopeEnvKey); err != nil || scope == "" {
		t.Skipf("kill scope not injected (tmux < 3.2?): %v", err)
	}

	var pid int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && pid == 0 {
		b, err := os.ReadFile(pidFile)
		if err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "GRANDCHILD: "); ok {
					pid, _ = strconv.Atoi(rest)
				}
			}
		}
		if pid == 0 {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if pid == 0 {
		t.Fatalf("pane helper never reported a daemonized grandchild")
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	inst := &Instance{ID: "test-killscope-e2e", Title: "killscope-e2e"}
	inst.tmuxSession = sess
	if err := inst.KillAndWait(); err != nil {
		t.Fatalf("KillAndWait: %v", err)
	}

	waitDead := time.Now().Add(3 * time.Second)
	for time.Now().Before(waitDead) && syscall.Kill(pid, 0) == nil {
		time.Sleep(50 * time.Millisecond)
	}
	if syscall.Kill(pid, 0) == nil {
		t.Fatalf("daemonized pane child %d survived Instance.KillAndWait — orphaned-executor regression", pid)
	}
}
