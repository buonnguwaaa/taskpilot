package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/db"
	"github.com/buonnguwaaa/taskpilot/internal/gate"
	"github.com/buonnguwaaa/taskpilot/internal/hasakiwork"
	"github.com/buonnguwaaa/taskpilot/internal/plugin"
	"github.com/buonnguwaaa/taskpilot/internal/splunk"
)

// cmdDoctor checks the config against the machine without changing anything
// (no fetch: it looks at remote-tracking refs that already exist).
func cmdDoctor(args []string) error {
	c := newFlags("doctor")
	c.fs.Parse(args)
	cfg, err := config.Load(*c.config)
	if err != nil {
		return err
	}
	problems := 0
	bad := func(format string, a ...any) {
		problems++
		fmt.Printf("  ✗ "+format+"\n", a...)
	}
	ok := func(format string, a ...any) { fmt.Printf("  ✓ "+format+"\n", a...) }

	fmt.Printf("config %s\n", cfg.Path)
	if _, err := gate.Compile(cfg.Rules); err != nil {
		bad("policy: %v", err)
	} else {
		ok("policy: %d rules, gates %s", len(cfg.Rules), strings.Join(cfg.Gates, ", "))
	}
	if p, err := exec.LookPath(cfg.Claude.Bin); err != nil {
		bad("claude binary %q not found", cfg.Claude.Bin)
	} else {
		ok("claude: %s", p)
	}

	d, err := db.Open(context.Background(), cfg.Home)
	if err != nil {
		bad("database: %v", err)
	} else {
		if cfg.ArgoCD.URL != "" {
			if line := argocdStatus(context.Background(), cfg, d); strings.Contains(line, "chưa đăng nhập") || strings.Contains(line, "HẾT HẠN") {
				bad("%s", line)
			} else {
				ok("%s", line)
			}
		}
		st, err := hasakiwork.ReadStatus(context.Background(), d)
		if err != nil {
			bad("hasaki work: %v", err)
		} else if !st.HasToken || st.Remaining(time.Now()) <= 0 {
			bad("%s", st.Summary(time.Now()))
		} else {
			ok("%s", st.Summary(time.Now()))
		}
		d.Close()
	}

	for _, pname := range cfg.ProjectNames() {
		proj := cfg.Projects[pname]
		hw := "not linked to Hasaki Work"
		if proj.HasakiProjectID != 0 {
			hw = fmt.Sprintf("Hasaki Work project %d", proj.HasakiProjectID)
		}
		fmt.Printf("\nproject %s — %s (%s, %d repos)\n", pname, proj.Name, hw, len(proj.Repos))
		for _, f := range proj.ContextFiles {
			if _, err := os.Stat(f); err != nil {
				bad("context file missing: %s", f)
			}
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		for _, name := range proj.RepoNames() {
			r := proj.Repos[name]
			var notes []string
			status := "✓"
			if out, err := exec.Command("git", "-C", r.Path, "rev-parse", "--show-toplevel").Output(); err != nil {
				status, notes = "✗", append(notes, "not a git repo: "+r.Path)
			} else if strings.TrimSpace(string(out)) != strings.TrimRight(r.Path, "/") {
				notes = append(notes, "path is inside "+strings.TrimSpace(string(out)))
			}
			if status == "✓" {
				ref := r.Base
				if r.Remote != "-" {
					ref = "refs/remotes/" + r.Remote + "/" + r.Base
				}
				if exec.Command("git", "-C", r.Path, "rev-parse", "--verify", "--quiet", ref).Run() != nil {
					status, notes = "✗", append(notes, "missing "+strings.TrimPrefix(ref, "refs/remotes/"))
				}
				if r.Remote != "-" {
					for _, st := range r.PRFlow {
						tref := "refs/remotes/" + r.Remote + "/" + st.Target
						if exec.Command("git", "-C", r.Path, "rev-parse", "--verify", "--quiet", tref).Run() != nil {
							status, notes = "✗", append(notes, "PR target "+r.Remote+"/"+st.Target+" not found (fetch, or set pr_flow for this repo)")
						}
					}
				}
			}
			for _, tool := range verifyTools(r) {
				if _, err := exec.LookPath(tool); err != nil {
					status, notes = "✗", append(notes, tool+" not in PATH")
				}
			}
			if r.Verify == "" {
				notes = append(notes, "no verify command")
			}
			if status == "✗" {
				problems++
			}
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\n", status, name, flowTargets(r), short(r.Verify, 40), strings.Join(notes, "; "))
		}
		w.Flush()
	}

	if lp := cfg.LogProjects(); len(lp) > 0 {
		fmt.Println("\nlogs production")
		for _, name := range lp {
			pl := cfg.Projects[name].LogsProduction
			if pl.Knowledge != "" {
				if _, err := plugin.Read("skills/prod-logs/" + pl.Knowledge); err != nil {
					bad("%s: knowledge file %s not found", name, pl.Knowledge)
				}
			}
			okQ := 0
			for _, q := range pl.Queries {
				vals := map[string]string{"filter": pl.Filter}
				for _, p := range splunk.Placeholders(q.SPL) {
					vals[p] = "x"
				}
				spl, err := splunk.Expand(q.SPL, vals)
				if err == nil {
					err = splunk.Validate(spl)
				}
				if err != nil {
					bad("%s: query %q: %v", name, q.Name, err)
					continue
				}
				okQ++
			}
			filter := pl.Filter
			if filter == "" {
				filter = "(chưa có filter prod)"
			}
			ok("%s: %d truy vấn hợp lệ · %s", name, okQ, filter)
		}
	}
	if problems > 0 {
		return fmt.Errorf("%d problem(s)", problems)
	}
	fmt.Println("\nall good")
	return nil
}

// verifyTools lists the executables the verify and prepare commands start with.
func verifyTools(r config.Repo) []string {
	seen := map[string]bool{}
	var out []string
	add := func(t string) {
		if t != "" && !seen[t] && !strings.Contains(t, "=") {
			seen[t] = true
			out = append(out, t)
		}
	}
	for _, part := range strings.FieldsFunc(r.Verify, func(r rune) bool { return r == '&' || r == '|' || r == ';' }) {
		fields := strings.Fields(part)
		for _, f := range fields {
			if !strings.Contains(f, "=") { // skip VAR=value prefixes
				add(f)
				break
			}
		}
	}
	for _, argv := range r.Prepare {
		if len(argv) > 0 {
			add(argv[0])
		}
	}
	return out
}
