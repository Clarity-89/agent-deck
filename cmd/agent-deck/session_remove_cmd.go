package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/asheshgoplani/agent-deck/internal/events"
	"github.com/asheshgoplani/agent-deck/internal/git"
	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/telemetry"
	"github.com/asheshgoplani/agent-deck/internal/vcs"
)

// handleSessionRemove deletes a session from the registry.
//
// By default only sessions in stopped/error state may be removed; --force
// bypasses the gate. --all-errored removes every session in error state.
// --prune-worktree additionally kills the tmux process and removes any git
// worktree associated with the session (registry-only by default), and
// --delete-branch then deletes the branch that worktree was on.
//
// Claude transcripts under ~/.claude/projects/<slug>/ are never touched.
func handleSessionRemove(profile string, args []string) {
	fs := flag.NewFlagSet("session remove", flag.ContinueOnError)
	jsonOutput := fs.Bool("json", false, "Output as JSON")
	quiet := fs.Bool("quiet", false, "Minimal output")
	quietShort := fs.Bool("q", false, "Minimal output (short)")
	force := fs.Bool("force", false, "Remove even when the session is running/waiting/idle; with --all-errored, also include pinned sessions (destructive)")
	allErrored := fs.Bool("all-errored", false, "Remove every unpinned session currently in the 'error' state (bulk); pinned sessions are skipped unless --force is given")
	pruneWorktree := fs.Bool("prune-worktree", false, "Also remove the session's git worktree (destructive: discards uncommitted work; a worktree another session uses is kept)")
	deleteBranch := fs.Bool("delete-branch", false, "With --prune-worktree, also delete the removed worktree's branch (destructive: unpushed commits are lost; the repository's default branch is kept)")

	fs.Usage = func() {
		fmt.Println("Usage: agent-deck session remove <id|title> [options]")
		fmt.Println("       agent-deck session remove --all-errored [options]")
		fmt.Println()
		fmt.Println("Remove a session from the registry. By default only stopped or")
		fmt.Println("errored sessions may be removed; use --force to bypass.")
		fmt.Println()
		fmt.Println("This is registry-only by default: Claude transcripts under")
		fmt.Println("~/.claude/projects/ are preserved. Pass --prune-worktree to also")
		fmt.Println("kill the process and delete the git worktree (destructive), and")
		fmt.Println("--delete-branch to delete the worktree's branch with it.")
		fmt.Println()
		fmt.Println("Options:")
		fs.PrintDefaults()
	}

	if err := parseCLIFlags(fs, normalizeArgs(fs, args)); err != nil {
		exitCLI(1)
	}

	quietMode := *quiet || *quietShort
	out := NewCLIOutput(*jsonOutput, quietMode)
	if *deleteBranch && !*pruneWorktree {
		out.Error("--delete-branch requires --prune-worktree", ErrCodeInvalidOperation)
		exitCLI(1)
	}

	storage, instances, groups, err := loadSessionData(profile)
	if err != nil {
		out.Error(err.Error(), ErrCodeInvalidOperation)
		exitCLI(1)
	}

	if *allErrored {
		removeAllErrored(out, storage, instances, groups, *pruneWorktree, *deleteBranch, *force)
		return
	}

	identifier := fs.Arg(0)
	if identifier == "" {
		out.Error("usage: session remove <id|title> OR --all-errored", ErrCodeInvalidOperation)
		exitCLI(1)
	}

	inst, errMsg, errCode := ResolveSession(identifier, instances)
	if inst == nil {
		out.Error(errMsg, errCode)
		if errCode == ErrCodeNotFound {
			exitCLI(2)
		}
		exitCLI(1)
		return
	}

	if !*force && !isRemovableStatus(inst.Status) {
		out.Error(
			fmt.Sprintf(
				"session '%s' is in state '%s'; only stopped/error sessions may be removed without --force",
				inst.Title, inst.Status,
			),
			ErrCodeInvalidOperation,
		)
		exitCLI(1)
	}

	// Always kill the tmux scope + its process tree before deleting the
	// registry row (issue #59, v1.7.68). Previously Kill() was only
	// called inside pruneSessionWorktree, so `session remove --force`
	// on a running session left the claude child running as an orphan
	// — observed on the maintainer's host as a 33-hour orphan claude
	// process with a since-deleted AGENTDECK_INSTANCE_ID.
	//
	// KillAndWait runs the SIGTERM→SIGKILL escalation synchronously so
	// the kill completes before this short-lived CLI exits.
	_ = killForRemoval(storage, inst)
	inst.RecordTelemetryEnd(telemetry.EndDelete)

	if *pruneWorktree {
		pruneSessionWorktree(inst, instances, *deleteBranch)
	}

	// v1.9.1 (#909): RemoveSessionAndVerify replaces the
	// DeleteInstance+saveSessionData pair. The old pair would silently
	// resurrect the row when a concurrent rewriter loaded the instance
	// list before our DELETE — exactly the "session remove --force
	// reports success but row stays" failure noted in the bug report.
	instances = dropInstance(instances, inst.ID)
	groupTree := session.NewGroupTreeWithGroups(instances, groups)
	if err := storage.RemoveSessionAndVerify(inst.ID, instances, groupTree); err != nil {
		out.Error(fmt.Sprintf("failed to remove session: %v", err), ErrCodeInvalidOperation)
		exitCLI(1)
	}

	// Best-effort cleanup for the removed row (issue #910 sweep,
	// macapp.session.closed) — see the matching block in handleRemove.
	session.CleanupRemovedSession(storage.Profile(), inst.ID)

	out.Success(fmt.Sprintf("Removed session: %s", inst.Title), map[string]interface{}{
		"success": true,
		"id":      inst.ID,
		"title":   inst.Title,
	})
}

// isRemovableStatus returns true for states where a session can be removed
// from the registry without --force.
func isRemovableStatus(s session.Status) bool {
	return s == session.StatusStopped || s == session.StatusError
}

// removedSessionRow is the {id,title} payload emitted for each removed session.
type removedSessionRow = map[string]interface{}

// removeAllErrored implements the --all-errored bulk path.
func removeAllErrored(
	out *CLIOutput,
	storage *session.Storage,
	instances []*session.Instance,
	groups []*session.GroupData,
	pruneWorktree bool,
	deleteBranch bool,
	force bool,
) {
	var doomed []*session.Instance
	skipped := 0
	for _, inst := range instances {
		if inst.Status != session.StatusError {
			continue
		}
		// pin-protects-from-stop: a pinned errored session is retained
		// unless --force is given.
		if inst.Pin != session.PinNone && !force {
			skipped++
			continue
		}
		doomed = append(doomed, inst)
	}

	removed := bulkRemoveSessions(out, storage, instances, groups, doomed, pruneWorktree, deleteBranch)

	msg := fmt.Sprintf("Removed %d errored session(s)", len(removed))
	if skipped > 0 {
		msg += fmt.Sprintf(" (skipped %d pinned — use --force to include)", skipped)
	}
	out.Success(msg, map[string]interface{}{
		"success": true,
		"count":   len(removed),
		"removed": removed,
		"skipped": skipped,
	})
}

// bulkRemoveSessions is the ONE implementation of the safety-critical bulk
// delete choreography, shared by `session remove --all-errored` and
// `session cleanup`. Keeping it single-sourced matters: the two copies it
// replaced had already drifted apart (only one did KillAndWait / pin-skip), and
// each of the invariants below was a separate production bug.
//
//   - KillAndWait before DELETE (#59, v1.7.68): an errored/dead session often
//     still owns a live agent child. Deleting the row without a synchronous
//     SIGTERM→SIGKILL escalation is how a 33-hour orphan claude process with a
//     since-deleted AGENTDECK_INSTANCE_ID was created.
//   - pruneWorktree is OPT-IN: it force-deletes the git worktree directory,
//     destroying uncommitted work. Never infer it. deleteBranch, opt-in too,
//     then deletes the branch a removed worktree was on.
//   - SaveGroupsOnly, never SaveWithGroups (#909): a full-table rewrite's
//     INSERT OR REPLACE resurrects the rows we just deleted. NewGroupTreeWithGroups
//     over the SURVIVING set also preserves groups whose last session just went
//     away (empty groups otherwise vanish forever).
//   - Verify + re-DELETE: a concurrent writer (a live TUI) can still resurrect a
//     row between our DELETE and our save.
//   - Inbox / notify-state sweep (#910).
//   - A worktree a surviving session still uses is kept (#1449). Sharing is
//     judged against the survivors, so sharers removed together still prune.
//
// Returns the {id,title} payload rows for the sessions actually removed.
func bulkRemoveSessions(
	out *CLIOutput,
	storage *session.Storage,
	instances []*session.Instance,
	groups []*session.GroupData,
	doomed []*session.Instance,
	pruneWorktree bool,
	deleteBranch bool,
) []removedSessionRow {
	doomedIDs := make(map[string]bool, len(doomed))
	for _, inst := range doomed {
		doomedIDs[inst.ID] = true
	}
	remaining := make([]*session.Instance, 0, len(instances)-len(doomed))
	for _, inst := range instances {
		if !doomedIDs[inst.ID] {
			remaining = append(remaining, inst)
		}
	}

	removed := make([]removedSessionRow, 0, len(doomed))
	removedIDs := make([]string, 0, len(doomed))
	for _, inst := range doomed {
		_ = killForRemoval(storage, inst)
		if pruneWorktree {
			pruneSessionWorktree(inst, remaining, deleteBranch)
		}
		if err := storage.DeleteInstance(inst.ID); err != nil {
			out.Error(fmt.Sprintf("failed to remove session %s: %v", inst.ID, err), ErrCodeInvalidOperation)
			exitBulkRemove(1)
		}
		// Clean up per deleted row, right away: a later failure exits, and
		// the rows already deleted must still be swept and announced.
		session.CleanupRemovedSession(storage.Profile(), inst.ID)
		removedIDs = append(removedIDs, inst.ID)
		removed = append(removed, map[string]interface{}{"id": inst.ID, "title": inst.Title})
	}

	groupTree := session.NewGroupTreeWithGroups(remaining, groups)
	if err := storage.SaveGroupsOnly(groupTree); err != nil {
		out.Error(fmt.Sprintf("failed to save session state: %v", err), ErrCodeInvalidOperation)
		exitBulkRemove(1)
	}

	for _, id := range removedIDs {
		if exists, _ := storage.InstanceExists(id); exists {
			_ = storage.DeleteInstance(id)
		}
	}
	return removed
}

// exitBulkRemove exits a bulk remove that failed part way. os.Exit skips
// main's deferred events drain, so the macapp.session.closed frames of the
// rows already deleted are flushed first.
func exitBulkRemove(code int) {
	_ = events.CloseDefault()
	exitCLI(code)
}

// warnRemovedSessionCleanup prints the best-effort cleanup outcome of a
// removed session to stderr (never in --json mode).
func warnRemovedSessionCleanup(r session.RemovedSessionCleanup, id string, jsonOutput bool) {
	if jsonOutput {
		return
	}
	if r.InboxErr != nil {
		fmt.Fprintf(os.Stderr, "warn: inbox sweep for %s failed: %v\n", id, r.InboxErr)
	} else if r.InboxSwept > 0 {
		fmt.Fprintf(os.Stderr, "swept %d stale inbox event(s) for removed session\n", r.InboxSwept)
	}
	if r.NotifyStateErr != nil {
		fmt.Fprintf(os.Stderr, "warn: notify-state sweep for %s failed: %v\n", id, r.NotifyStateErr)
	}
}

// killForRemoval kills inst before a removal deletes its row, and saves the
// row as stopped right after the kill, as a stop does (defect 317). The row
// is deleted only after the kill and, with a worktree, after `git worktree
// remove`, which can take longer than the notify daemon's 5 s death grace on
// a big tree (#2806). A stopped row is a deliberate end, so the daemon never
// tells the parent the child "exited while idle". Saved before the kill, the
// stopped row with its pane still alive read to the daemon as a start: it
// wrote waiting, then error once the pane went, and the row stayed error
// until it was deleted. An error the daemon samples during the kill itself
// is replaced here.
func killForRemoval(storage *session.Storage, inst *session.Instance) error {
	pre := session.BeforeStopKill(storage, inst.ID)
	err := inst.KillAndWait()
	session.WriteStoppedAfterKill(storage, inst, pre)
	return err
}

// pruneSessionWorktree kills the session and removes its git worktree (if any),
// and with deleteBranch the branch the worktree was on. Errors are logged to
// stderr but never block the remove; a worktree or branch that stays behind is
// always said so on stderr, so a caller reading it can tell a kept one from a
// removed one.
//
// Uses KillAndWait so the SIGTERM→SIGKILL escalation completes before
// this short-lived CLI exits (issue #59, v1.7.68).
//
// A worktree another session in others still works in is kept (#1449), as the
// TUI's delete keeps it: removing it would strand that session.
func pruneSessionWorktree(inst *session.Instance, others []*session.Instance, deleteBranch bool) {
	_ = inst.KillAndWait()
	if !inst.IsWorktree() {
		return
	}
	if session.OtherSessionsShareWorktree(inst, others) {
		fmt.Fprintf(os.Stderr, "note: worktree %s kept: another session still uses it\n", FormatPath(inst.WorktreePath))
		return
	}
	backend, err := worktreeBackend(inst)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: worktree remove failed for %s: %v\n", inst.ID, err)
		return
	}
	if removeWorktree(inst, backend) && deleteBranch {
		deleteWorktreeBranch(inst, backend)
	}
}

// removeWorktree removes inst's worktree and reports whether its directory is
// gone, which frees its branch. A worktree_reuse row points at the main
// working tree, which stays (#1200); git would only refuse it, and that
// refusal is not a failure. A directory already gone leaves only its admin
// entry, which prune takes.
func removeWorktree(inst *session.Instance, backend vcs.Backend) bool {
	if backend.Type() == vcs.TypeGit && !git.IsLinkedWorktree(inst.WorktreePath) {
		_ = backend.PruneWorktrees()
		_, err := os.Stat(inst.WorktreePath)
		return os.IsNotExist(err)
	}
	err := backend.RemoveWorktree(inst.WorktreePath, true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: worktree remove failed for %s: %v\n", inst.ID, err)
	}
	_ = backend.PruneWorktrees()
	return err == nil
}

// deleteWorktreeBranch deletes the branch inst's worktree was on. Forced: a
// branch merged by squash is no ancestor of the branch it went into, and
// the caller asked for the branch to go. The repository's default branch is
// kept, and git refuses a branch checked out in another worktree; a branch
// already gone is nothing to do. A row from before the branch was recorded
// has nothing to delete, which is said, so the caller does not take the
// branch for gone.
func deleteWorktreeBranch(inst *session.Instance, backend vcs.Backend) {
	branch := inst.WorktreeBranch
	if branch == "" {
		fmt.Fprintf(os.Stderr, "warn: branch delete failed for %s: no branch recorded for the worktree\n", inst.ID)
		return
	}
	if !backend.BranchExists(branch) {
		return
	}
	if defaultBranch, err := backend.GetDefaultBranch(); err == nil && defaultBranch == branch {
		fmt.Fprintf(os.Stderr, "note: branch %s kept: the repository's default branch\n", branch)
		return
	}
	if err := backend.DeleteBranch(branch, true); err != nil {
		fmt.Fprintf(os.Stderr, "warn: branch delete failed for %s: %v\n", inst.ID, err)
	}
}

// worktreeBackend is the backend that removes inst's worktree: the recorded
// repository root's, or the worktree's own when that root is empty, on a row
// from before it was stored, or stale once the repository moved. The
// worktree still knows its repository either way.
func worktreeBackend(inst *session.Instance) (vcs.Backend, error) {
	if inst.WorktreeRepoRoot != "" {
		if backend, err := detectAndCreateBackend(inst.WorktreeRepoRoot); err == nil {
			return backend, nil
		}
	}
	return detectAndCreateBackend(inst.WorktreePath)
}

// dropInstance returns a new slice with the given id filtered out.
func dropInstance(instances []*session.Instance, id string) []*session.Instance {
	out := instances[:0]
	for _, i := range instances {
		if i.ID != id {
			out = append(out, i)
		}
	}
	return out
}
