package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/buonnguwaaa/taskpilot/internal/oneshot"
)

// Whether the operator asked for a merge is decided from what they meant,
// not from a word: "resolve the conflicts of the PR that goes into QC" is
// not "merge it". A one-shot Claude call reads the operator's text of this
// turn; anything unclear counts as no.

const mergeIntentSchema = `{"type":"object","properties":{"merge":{"type":"boolean"},"repos":{"type":"array","items":{"type":"string"}},"reason":{"type":"string"}},"required":["merge","repos","reason"],"additionalProperties":false}`

const mergeIntentSystem = `You read the messages a software operator sent an AI agent and decide whether they instruct it to MERGE pull requests now (complete the merge into the target branch), and for which repositories.

merge=true only when the operator clearly tells the agent to perform the merge itself: e.g. "merge PR frontend vào qc", "merge đi", "gộp 2 PR đó vào QC", "resolve conflict rồi merge luôn", "merge it for me".
merge=false when the operator only asks to resolve or fix conflicts, prepare, rebase, update, check whether it can be merged, open or update a PR, or asks a question; when the words "merge"/"gộp" only describe the PR or the conflict (e.g. "resolve PR frontend merge vào QC" = resolve the conflicts of the frontend PR that merges into QC; "fix conflict merge"); when it is negated ("đừng merge") or conditional on something not yet confirmed; or when you are unsure.

repos: the repositories, from the list given, whose PRs the operator tells to merge. Map what they wrote to the list ("frontend", "FE" → the repo whose name says frontend; "backend", "BE", "gateway" likewise). Use ["ALL"] when the instruction names no repository or covers all/both/"2 PR đó" without saying which. Empty when merge=false.
reason: one short sentence in the operator's language quoting the words that decided.`

// mergeIntentTimeout bounds the classification.
const mergeIntentTimeout = 90 * time.Second

// claudeMergeIntent asks Claude whether texts instruct merging repo into
// target. Claude names the repositories the instruction covers; whether
// repo is one of them is checked here.
func (h *Handlers) claudeMergeIntent(ctx context.Context, texts []string, repo, target string) (bool, string, error) {
	repos := []string{repo}
	if t, err := h.Store.LoadTask(h.TaskID); err == nil {
		if p, ok := h.Config.Projects[t.Project]; ok {
			repos = p.RepoNames()
		}
	}
	ctx, cancel := context.WithTimeout(ctx, mergeIntentTimeout)
	defer cancel()
	c := h.Config.Claude
	args := oneshot.Args(mergeIntentSchema, mergeIntentSystem, h.Config.Claude.ClassifyModel, c.SettingSources)
	var b strings.Builder
	fmt.Fprintf(&b, "Repositories of the task: %s\nTarget branch: %s\n\nThe operator's messages that started this turn:\n",
		strings.Join(repos, ", "), target)
	for _, t := range texts {
		fmt.Fprintf(&b, "<<<\n%s\n>>>\n", oneshot.Clip(t, 3000))
	}
	bin := c.Bin
	if bin == "" {
		return false, "", errors.New("claude.bin is not set")
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = os.TempDir()
	cmd.Stdin = strings.NewReader(b.String())
	// A plain one-shot call, not a child of the agent's session.
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CLAUDECODE=") && !strings.HasPrefix(kv, "CLAUDE_CODE_ENTRYPOINT=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return false, "", fmt.Errorf("claude: %w: %s", err, oneshot.Clip(strings.TrimSpace(stderr.String()), 300))
	}
	raw, _, err := oneshot.Answer(out)
	if err != nil {
		return false, "", err
	}
	var a struct {
		Merge  bool     `json:"merge"`
		Repos  []string `json:"repos"`
		Reason string   `json:"reason"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return false, "", err
	}
	if !a.Merge {
		return false, a.Reason, nil
	}
	for _, r := range a.Repos {
		if r == "ALL" || strings.EqualFold(strings.TrimSpace(r), repo) {
			return true, a.Reason, nil
		}
	}
	return false, fmt.Sprintf("%s (the merge asked is for %s, not %s)", a.Reason, strings.Join(a.Repos, ", "), repo), nil
}
