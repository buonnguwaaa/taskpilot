package scan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/buonnguwaaa/taskpilot/internal/oneshot"
	"github.com/buonnguwaaa/taskpilot/internal/splunk"
)

// A flow is error groups that belong to one failing path: one error leads to
// the others (a call to a partner times out, then the caller logs its own
// failure, then the worker retrying it...). The operator acts on the flow,
// not on each log line.
//
// Two passes:
//  1. evidence: groups logged in the same requests or transactions
//     (splunk.LinkSPL) are always in the same flow;
//  2. Claude (optional) reads the groups and joins the ones of one path that
//     share no id, e.g. a worker's gRPC error and the service it called.

// Flow is one failing path over the groups of a scan.
type Flow struct {
	Title   string `json:"title"`   // short name of the path ("" = the main group's message)
	Cause   string `json:"cause"`   // likely cause, in a sentence ("" when unknown)
	How     string `json:"-"`       // "request_id" (shared ids), "claude", or "" (one group)
	Members []int  `json:"members"` // indexes in the scan's groups, most frequent first
}

// GroupInput is what the flow grouper reads.
type GroupInput struct {
	Project  string
	Language string
	Groups   []splunk.ErrorGroup
	Linked   [][]int // the flows proven by shared ids (pass 1)
}

// Grouper proposes flows (pass 2).
type Grouper func(ctx context.Context, in GroupInput) ([]Flow, error)

// linked reports whether groups sharing ids are one flow: they share at
// least 2 requests, or half of the smaller group (one shared id can be chance).
func linked(l splunk.Link, counts []int) bool {
	small := 0
	for _, c := range counts {
		if small == 0 || c < small {
			small = c
		}
	}
	return l.Shared >= 2 || (small > 0 && l.Shared*2 >= small)
}

// linkFlows unions the groups linked by shared ids; it returns every group
// in exactly one set (singletons included), biggest first.
func linkFlows(groups []splunk.ErrorGroup, links []splunk.Link) [][]int {
	idx := map[[2]string]int{}
	for i, g := range groups {
		idx[[2]string{g.Service, g.Signature}] = i
	}
	u := newUnion(len(groups))
	for _, l := range links {
		var members []int
		var counts []int
		for _, k := range l.Groups {
			if i, ok := idx[k]; ok {
				members = append(members, i)
				counts = append(counts, groups[i].Count)
			}
		}
		if len(members) < 2 || !linked(l, counts) {
			continue
		}
		for _, i := range members[1:] {
			u.join(members[0], i)
		}
	}
	return u.sets(groups)
}

// buildFlows merges the proven sets with the grouper's proposal: a proposal
// never splits a proven set, and a group it leaves out stays on its own.
func buildFlows(groups []splunk.ErrorGroup, proven [][]int, proposed []Flow) []Flow {
	u := newUnion(len(groups))
	for _, s := range proven {
		for _, i := range s[1:] {
			u.join(s[0], i)
		}
	}
	type meta struct{ title, cause string }
	var metas []struct {
		members []int
		meta
	}
	for _, f := range proposed {
		var ok []int
		for _, i := range f.Members {
			if i >= 0 && i < len(groups) {
				ok = append(ok, i)
			}
		}
		for _, i := range ok[min(1, len(ok)):] {
			u.join(ok[0], i)
		}
		if len(ok) > 0 {
			metas = append(metas, struct {
				members []int
				meta
			}{ok, meta{strings.TrimSpace(f.Title), strings.TrimSpace(f.Cause)}})
		}
	}
	provenRoot := map[int]bool{}
	for _, s := range proven {
		if len(s) > 1 {
			provenRoot[u.find(s[0])] = true
		}
	}
	var out []Flow
	for _, s := range u.sets(groups) {
		f := Flow{Members: s}
		root := u.find(s[0])
		// The proposal that covers most of the set names it.
		best := 0
		for _, m := range metas {
			n := 0
			for _, i := range m.members {
				if u.find(i) == root {
					n++
				}
			}
			if n > best && (m.title != "" || m.cause != "") {
				best, f.Title, f.Cause = n, m.title, m.cause
			}
		}
		switch {
		case len(s) == 1:
		case provenRoot[root] && !joinedBeyond(s, proven):
			f.How = "request_id"
		default:
			f.How = "claude"
		}
		out = append(out, f)
	}
	return out
}

// joinedBeyond reports whether set s holds more than one proven set (or a
// group outside them): something else than shared ids joined it.
func joinedBeyond(s []int, proven [][]int) bool {
	for _, p := range proven {
		if len(p) > 1 && contains(p, s[0]) {
			return len(p) != len(s)
		}
	}
	return true
}

func contains(s []int, x int) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}

// ---- union-find --------------------------------------------------------------------

type union []int

func newUnion(n int) union {
	u := make(union, n)
	for i := range u {
		u[i] = i
	}
	return u
}

func (u union) find(i int) int {
	for u[i] != i {
		u[i] = u[u[i]]
		i = u[i]
	}
	return i
}

func (u union) join(a, b int) {
	ra, rb := u.find(a), u.find(b)
	if ra == rb {
		return
	}
	if rb < ra {
		ra, rb = rb, ra
	}
	u[rb] = ra
}

// sets lists the sets, members most frequent first, sets by total count.
func (u union) sets(groups []splunk.ErrorGroup) [][]int {
	by := map[int][]int{}
	var roots []int
	for i := range u {
		r := u.find(i)
		if _, ok := by[r]; !ok {
			roots = append(roots, r)
		}
		by[r] = append(by[r], i)
	}
	total := func(s []int) int {
		n := 0
		for _, i := range s {
			n += groups[i].Count
		}
		return n
	}
	var out [][]int
	for _, r := range roots {
		s := by[r]
		sort.SliceStable(s, func(a, b int) bool { return groups[s[a]].Count > groups[s[b]].Count })
		out = append(out, s)
	}
	sort.SliceStable(out, func(a, b int) bool { return total(out[a]) > total(out[b]) })
	return out
}

// ---- Claude grouper ----------------------------------------------------------------

// GroupTimeout bounds the Claude pass.
const GroupTimeout = 3 * time.Minute

const flowSchema = `{
  "type": "object",
  "properties": {
    "flows": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "title": {"type": "string"},
          "cause": {"type": "string"},
          "members": {"type": "array", "items": {"type": "integer"}}
        },
        "required": ["title", "cause", "members"],
        "additionalProperties": false
      }
    }
  },
  "required": ["flows"],
  "additionalProperties": false
}`

const flowSystem = `You group production error log groups of one product into failing flows, for an engineer deciding what to fix. You have no tools: answer with the JSON object only.
A flow is one failing path: the errors one root problem produces along a call chain (a partner/API/DB call fails or times out, the service that made the call logs its own error, the gRPC caller or the worker retrying it logs another, the job reporting it logs another). Put groups in the same flow when they are links of the same chain or the same failing operation (same partner endpoint, same request type, same failing dependency, matching counts and times). Keep unrelated failures apart even if their wording is similar; do not group just because two errors are in the same service.
Rules: every group number appears in exactly one flow; groups listed together under "linked" share request ids and MUST stay in the same flow; a group with nothing related is a flow of its own.
title: at most 80 characters, in the operator's language, naming the failing operation (e.g. "ZaloPay: truy vấn trạng thái đơn bị timeout"). cause: one sentence, the most likely root cause from the evidence, or "" if unclear; never invent facts.`

// ClaudeGrouper runs one short Claude turn without tools.
type ClaudeGrouper struct {
	Bin            string
	Model          string
	SettingSources string
	// Run executes claude (tests); nil = exec Bin.
	Run func(ctx context.Context, args []string, stdin string) ([]byte, error)
}

// Group implements Grouper.
func (c *ClaudeGrouper) Group(ctx context.Context, in GroupInput) ([]Flow, error) {
	if len(in.Groups) < 2 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(oneshot.WithPurpose(ctx, "gom nhóm lỗi"), GroupTimeout)
	defer cancel()
	run := c.Run
	if run == nil {
		run = func(ctx context.Context, args []string, stdin string) ([]byte, error) {
			return oneshot.Exec(ctx, c.Bin, args, stdin, GroupTimeout)
		}
	}
	out, err := run(ctx, oneshot.Args(flowSchema, flowSystem, c.Model, c.SettingSources), GroupPrompt(in))
	if err != nil {
		return nil, err
	}
	return parseFlows(out)
}

// GroupPrompt lays out the groups (exported for tests).
func GroupPrompt(in GroupInput) string {
	var b strings.Builder
	lang := in.Language
	if lang == "" {
		lang = "Vietnamese"
	}
	fmt.Fprintf(&b, "Operator language: %s\nProject: %s\n\nError groups (number, container, count, first → last seen, message with numbers masked):\n", lang, in.Project)
	for i, g := range in.Groups {
		fmt.Fprintf(&b, "\n[%d] %s · %d× · %s → %s\n    message: %s\n", i, g.Service, g.Count,
			g.First.Local().Format("01-02 15:04"), g.Last.Local().Format("01-02 15:04"), oneshot.Clip(g.Signature, 200))
		if g.Sample != "" && g.Sample != g.Signature {
			fmt.Fprintf(&b, "    example: %s\n", oneshot.Clip(g.Sample, 300))
		}
		if g.Caller != "" || g.Prefix != "" {
			fmt.Fprintf(&b, "    caller: %s  function: %s\n", g.Caller, g.Prefix)
		}
		if st := strings.TrimSpace(g.Stacktrace); st != "" {
			lines := strings.Split(st, "\n")
			fmt.Fprintf(&b, "    stack: %s\n", oneshot.Clip(strings.Join(lines[:min(3, len(lines))], " | "), 300))
		}
	}
	var linked []string
	for _, s := range in.Linked {
		if len(s) > 1 {
			parts := make([]string, len(s))
			for i, x := range s {
				parts[i] = fmt.Sprint(x)
			}
			linked = append(linked, "["+strings.Join(parts, ", ")+"]")
		}
	}
	if len(linked) > 0 {
		b.WriteString("\nlinked (share request ids / transaction codes, must stay together): " + strings.Join(linked, " ") + "\n")
	}
	return b.String()
}

func parseFlows(out []byte) ([]Flow, error) {
	raw, _, err := oneshot.Answer(out)
	if err != nil {
		return nil, err
	}
	var v struct {
		Flows []Flow `json:"flows"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, errors.New("claude returned no valid flows")
	}
	return v.Flows, nil
}
