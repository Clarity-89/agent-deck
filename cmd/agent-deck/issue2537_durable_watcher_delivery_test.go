package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Issue #2537: a routed watcher event waits in the send queue for a stopped
// conductor, survives the exit of the process that routed it, and reaches
// the conductor once when it is back. Real binary, on the #2530 test env.

// killDetached removes sandbox workers that outlive their tmux panes.
func (e *issue2530Env) killDetached() {
	pids, err := processesWithEnvironment("HOME=" + filepath.Join(e.root, "home"))
	if err != nil {
		e.t.Errorf("find detached sandbox processes: %v", err)
		return
	}
	for _, pid := range pids {
		if pid > 1 && pid != os.Getpid() && pid != os.Getppid() {
			if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
				e.t.Errorf("kill detached sandbox process %d: %v", pid, err)
			}
		}
	}
}

// macOS ps combines argv and environment. Reject argv matches so a command
// merely mentioning the sandbox's HOME cannot be killed.
func processesWithEnvironment(entry string) ([]int, error) {
	var pids []int
	if runtime.GOOS == "darwin" {
		withEnv, err := exec.Command("/bin/ps", "-xww", "-E", "-o", "pid=,command=").Output()
		if err != nil {
			return nil, fmt.Errorf("read process environments: %w", err)
		}
		argvOnly, err := exec.Command("/bin/ps", "-xww", "-o", "pid=,command=").Output()
		if err != nil {
			return nil, fmt.Errorf("read process arguments: %w", err)
		}
		candidates := make(map[int]bool)
		for _, line := range strings.Split(string(withEnv), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 || !slices.Contains(fields[1:], entry) {
				continue
			}
			if pid, err := strconv.Atoi(fields[0]); err == nil {
				candidates[pid] = true
			}
		}
		for _, line := range strings.Split(string(argvOnly), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			if pid, err := strconv.Atoi(fields[0]); err == nil && candidates[pid] && !slices.Contains(fields[1:], entry) {
				pids = append(pids, pid)
			}
		}
		return pids, nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("list process environments: %w", err)
	}
	for _, ent := range entries {
		pid, err := strconv.Atoi(ent.Name())
		if err != nil {
			continue
		}
		env, err := os.ReadFile(filepath.Join("/proc", ent.Name(), "environ"))
		if err == nil && slices.Contains(strings.Split(string(env), "\x00"), entry) {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func TestDetachedProcessHelper(t *testing.T) {
	if os.Getenv("AGENTDECK_TEST_DETACHED_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	// A failed cleanup assertion must not leave an unbounded child.
	time.Sleep(30 * time.Second)
}

func TestKillDetachedStopsOnlySandboxProcesses(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	start := func(home string, arguments ...string) (*exec.Cmd, <-chan struct{}) {
		t.Helper()
		// Apple platform binaries hide their environment from ps. Re-exec
		// this binary, as the workers being cleaned up are Go binaries too.
		cmd := exec.Command(exe, append([]string{"-test.run=^TestDetachedProcessHelper$", "--"}, arguments...)...)
		cmd.Env = append(os.Environ(), "HOME="+home, "AGENTDECK_TEST_DETACHED_HELPER=1")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
		return cmd, done
	}
	target, done := start(home)
	decoy, _ := start(home+"-sibling", "HOME="+home)
	pids, err := processesWithEnvironment("HOME=" + home)
	if err != nil || !slices.Equal(pids, []int{target.Process.Pid}) {
		t.Fatalf("sandbox process lookup: pids %v, error %v; want [%d]", pids, err, target.Process.Pid)
	}
	e := &issue2530Env{t: t, root: root}
	e.killDetached()
	select {
	case <-done:
		if target.ProcessState.Success() {
			t.Fatal("sandbox worker exited without being killed")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("sandbox worker survived cleanup")
	}
	if err := syscall.Kill(decoy.Process.Pid, 0); err != nil {
		t.Fatalf("cleanup killed a process that only mentioned the sandbox in argv: %v", err)
	}
}

type issue2537Undelivered struct {
	Conductor string `json:"conductor"`
	Count     int    `json:"count"`
}

// undelivered reads `watcher status hook --json`'s undelivered list.
func (e *issue2530Env) undelivered() []issue2537Undelivered {
	e.t.Helper()
	var st struct {
		Undelivered []issue2537Undelivered `json:"undelivered"`
	}
	if err := json.Unmarshal([]byte(e.cli("watcher", "status", "hook", "--json")), &st); err != nil {
		e.t.Fatalf("watcher status --json: %v", err)
	}
	return st.Undelivered
}

func (e *issue2530Env) pane(tmuxSession string) string {
	capture := exec.Command("tmux", "capture-pane", "-p", "-J", "-S", "-", "-t", tmuxSession)
	capture.Env = e.env
	out, _ := capture.Output()
	return string(out)
}

// TestIssue2537_EventsForAStoppedConductorAreDeliveredOnceAfterQuit is the
// maintainer's test (a): events are routed while the conductor is stopped,
// the process that routed them quits, the conductor comes back, and it gets
// each event exactly once, in order.
func TestIssue2537_EventsForAStoppedConductorAreDeliveredOnceAfterQuit(t *testing.T) {
	parallelSubprocessTest(t)
	e := newIssue2530Env(t)
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	e.env = append(e.env,
		"AGENTDECK_SEND_WORKER_POLL=200ms",
		"AGENTDECK_SEND_RETRY_BACKOFF_MAX=500ms",
		"AGENTDECK_SEND_LAND_WINDOW=2s",
	)
	t.Cleanup(e.killDetached)
	e.createWebhookWatcher()

	var conductor struct {
		ID string `json:"id"`
	}
	out := e.cli("launch", filepath.Join(e.root, "proj"), "-t", "conductor-demo", "-c", "cat >/dev/null", "--no-parent", "--json")
	if err := json.Unmarshal([]byte(out), &conductor); err != nil || conductor.ID == "" {
		t.Fatalf("launch conductor-demo: %v\n%s", err, out)
	}
	e.cli("session", "stop", conductor.ID)

	web := e.startWeb("web")
	issue2530WaitFor(t, 10*time.Second, "the watcher port to listen", e.listening)
	stamp := strconv.FormatInt(time.Now().UnixNano(), 36)
	first, second := "first-2537-"+stamp, "second-2537-"+stamp
	e.post(first)
	e.post(second)
	e.waitRecorded(second)
	issue2530WaitFor(t, 10*time.Second, "both events queued for the stopped conductor", func() bool {
		u := e.undelivered()
		return len(u) == 1 && u[0].Conductor == "demo" && u[0].Count == 2
	})

	// The process that routed the events quits.
	if err := web.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	issue2530WaitFor(t, 15*time.Second, "web to exit on SIGTERM", func() bool {
		return syscall.Kill(web.Process.Pid, 0) != nil
	})
	time.Sleep(time.Second)
	if u := e.undelivered(); len(u) != 1 || u[0].Count != 2 {
		t.Fatalf("after quit the queue holds %+v, want both events for demo", u)
	}

	// The conductor comes back.
	e.cli("session", "start", conductor.ID)
	var shown struct {
		TmuxSession string `json:"tmux_session"`
	}
	if err := json.Unmarshal([]byte(e.cli("session", "show", conductor.ID, "--json")), &shown); err != nil || shown.TmuxSession == "" {
		t.Fatalf("session show: %v (%q)", err, shown.TmuxSession)
	}
	wantFirst := "[webhook] alice@example.com: " + first
	wantSecond := "[webhook] alice@example.com: " + second
	issue2530WaitFor(t, 30*time.Second, "both events in the conductor pane", func() bool {
		p := e.pane(shown.TmuxSession)
		return strings.Contains(p, wantFirst) && strings.Contains(p, wantSecond)
	})
	issue2530WaitFor(t, 30*time.Second, "the queue to empty", func() bool { return len(e.undelivered()) == 0 })
	time.Sleep(2 * time.Second)
	p := e.pane(shown.TmuxSession)
	if n := strings.Count(p, wantFirst); n != 1 {
		t.Fatalf("first event typed %d times, want once:\n%s", n, p)
	}
	if n := strings.Count(p, wantSecond); n != 1 {
		t.Fatalf("second event typed %d times, want once:\n%s", n, p)
	}
	if strings.Index(p, wantFirst) > strings.Index(p, wantSecond) {
		t.Fatalf("events typed out of order:\n%s", p)
	}
}
