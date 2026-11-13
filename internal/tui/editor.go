package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/buonnguwaaa/taskpilot/internal/store"
	"github.com/buonnguwaaa/taskpilot/internal/workspace"
)

// openEditor starts the IDE on dir without waiting for it. editor is the
// configured command (may carry arguments); empty: cursor, then code.
func openEditor(editor, dir string) error {
	argv := strings.Fields(editor)
	if len(argv) == 0 {
		for _, c := range []string{"cursor", "code"} {
			if p, err := exec.LookPath(c); err == nil {
				argv = []string{p}
				break
			}
		}
	}
	if len(argv) == 0 {
		return errors.New("không tìm thấy cursor hay code trong PATH; đặt `editor:` trong config")
	}
	cmd := exec.Command(argv[0], append(argv[1:], dir)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("mở %s: %w", argv[0], err)
	}
	go cmd.Wait() // reap; the IDE keeps running on its own
	return nil
}

// openWorkspace opens the selected task's workspace in the IDE: every repo
// the task opened is a git worktree on the task's branch, so its changes can
// be read there without checking the branch out anywhere else.
func (m Model) openWorkspace(t *store.Task) (tea.Model, tea.Cmd) {
	if t == nil {
		return m, nil
	}
	repos := workspace.OpenedRepos(t.Workspace)
	if t.Workspace == "" || len(repos) == 0 {
		if _, err := os.Stat(t.Workspace); t.Workspace != "" && err == nil {
			m.msg = "workspace chưa mở repo nào (hoặc worktree đã bị gỡ ở bản cũ): xem nhánh " + t.Branch + " trong repo của bạn"
		} else {
			m.msg = "task chưa có workspace"
		}
		return m, nil
	}
	dir := t.Workspace
	if len(repos) == 1 {
		dir = t.Workspace + "/" + repos[0]
	}
	if err := m.d.OpenEditor(dir); err != nil {
		m.msg = err.Error()
		return m, nil
	}
	m.msg = fmt.Sprintf("đã mở %s (%s) trong IDE", dir, strings.Join(repos, ", "))
	if t.State == store.StateRunning || t.State == store.StatePreparing {
		m.msg += " — agent đang chạy: chỉ xem, đừng sửa file lúc này"
	}
	return m, nil
}

// reviewTarget is one repo of a task to look at in the operator's clone.
type reviewTarget struct {
	repo, clone, branch string
}

// reviewTargets lists the task's repos with the branch to look at: the one
// its worktree is on (the agent may have renamed it), else the task branch
// for repos where it exists (worktrees already released).
func (m Model) reviewTargets(t *store.Task) []reviewTarget {
	proj := m.d.Config.Projects[t.Project]
	var out []reviewTarget
	opened := workspace.OpenedRepos(t.Workspace)
	for _, name := range opened {
		r, ok := proj.Repos[name]
		if !ok {
			continue
		}
		br := t.Branch
		if b, err := exec.Command("git", "-C", t.Workspace+"/"+name, "branch", "--show-current").Output(); err == nil && strings.TrimSpace(string(b)) != "" {
			br = strings.TrimSpace(string(b))
		}
		out = append(out, reviewTarget{repo: name, clone: r.Path, branch: br})
	}
	if len(out) > 0 || t.Branch == "" {
		return out
	}
	for _, name := range proj.RepoNames() {
		r := proj.Repos[name]
		if exec.Command("git", "-C", r.Path, "rev-parse", "--verify", "--quiet", t.Branch).Run() == nil {
			out = append(out, reviewTarget{repo: name, clone: r.Path, branch: t.Branch})
		}
	}
	return out
}

// reviewInClone puts the operator's own clones on the task's latest commits
// (detached HEAD: git refuses the branch itself while the task's worktree
// holds it). Pressed again: follows new commits, or returns to the previous
// branch when already up to date.
func (m Model) reviewInClone(t *store.Task) (tea.Model, tea.Cmd) {
	if t == nil {
		return m, nil
	}
	targets := m.reviewTargets(t)
	if len(targets) == 0 {
		m.msg = "task chưa có nhánh nào trong các repo (agent chưa mở repo)"
		return m, nil
	}
	m.msg = "đang chuyển repo của bạn sang code của task…"
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		var parts []string
		for _, tg := range targets {
			act, err := workspace.ReviewCheckout(ctx, tg.clone, tg.branch)
			switch {
			case err != nil:
				parts = append(parts, tg.repo+": "+err.Error())
			case act == workspace.ReviewReturned:
				parts = append(parts, tg.repo+": đã về lại nhánh cũ")
			case act == workspace.ReviewUpdated:
				parts = append(parts, tg.repo+": cập nhật lên commit mới nhất của "+tg.branch)
			default:
				parts = append(parts, tg.repo+": đang xem "+tg.branch+" (detached)")
			}
		}
		return infoMsg(strings.Join(parts, " · ") + " — W lần nữa: cập nhật / quay về nhánh cũ")
	}
}
