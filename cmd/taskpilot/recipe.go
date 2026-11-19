package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/buonnguwaaa/taskpilot/internal/argocd"
	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/kafbat"
	"github.com/buonnguwaaa/taskpilot/internal/mcpserver"
	"github.com/buonnguwaaa/taskpilot/internal/recipe"
	"github.com/buonnguwaaa/taskpilot/internal/splunk"
	"github.com/buonnguwaaa/taskpilot/internal/store"
	"github.com/buonnguwaaa/taskpilot/internal/vault"
)

const recipeUsage = `taskpilot recipe <command>    (debugging recipes: <home>/recipes/NAME.yaml)

  list [PROJECT]                 recipes, and proposals waiting for you
  show NAME                      the recipe as YAML
  run NAME [param=value …]       run it now (read-only lookups, no Claude)
  run -file FILE [param=value …] try a YAML file before adding it
  check FILE                     validate a YAML file without saving it
  add FILE                       validate and save a hand-written recipe
  rm NAME                        delete a recipe
`

const reflectUsage = `taskpilot reflect [-project P] [-days N] [-dry]   propose recipes from the tasks since the last reflection
taskpilot reflect list [PROJECT]             proposals waiting for you
taskpilot reflect show ID                    one proposal: why, evidence, YAML
taskpilot reflect accept ID | reject ID      save it as a recipe / do not propose it again
`

// operatorHandlers runs the read-only agent lookups for the operator, scoped
// to a project (no task, no audit rows).
func operatorHandlers(cfg *config.Config, st *store.Store, project, who string) *mcpserver.Handlers {
	return &mcpserver.Handlers{Store: st, Config: cfg, Project: project,
		Splunk: func(ctx context.Context) (*splunk.Client, error) { return splunkClient(ctx, cfg, who) },
		ArgoCD: func(ctx context.Context) (*argocd.Client, error) { return argocdClient(ctx, cfg, who) },
		Vault:  func(ctx context.Context) (*vault.Client, error) { return vaultClient(ctx, cfg, who) },
		Kafka: func(ctx context.Context) (*kafbat.Client, string, error) {
			cl, err := kafkaClient(ctx, cfg, who)
			if err != nil {
				return nil, "", err
			}
			cluster, err := kafkaCluster(ctx, cfg, cl)
			return cl, cluster, err
		},
		Secret: secretReader(cfg, who)}
}

// runRecipe runs a recipe as the operator and returns its formatted results.
func runRecipe(ctx context.Context, cfg *config.Config, st *store.Store, r *recipe.Recipe, vals map[string]string) (string, error) {
	if err := recipe.Validate(r, knownProject(cfg)); err != nil {
		return "", err
	}
	res, err := recipe.Run(ctx, r, vals, mcpserver.RecipeExec{H: operatorHandlers(cfg, st, r.Project, "operator:recipe")})
	if err != nil {
		return "", err
	}
	return recipe.Format(r, vals, res), nil
}

func knownProject(cfg *config.Config) func(string) bool {
	return func(p string) bool { _, ok := cfg.Projects[p]; return ok }
}

func cmdRecipe(args []string) error {
	if len(args) == 0 {
		fmt.Print(recipeUsage)
		return nil
	}
	sub, args := args[0], args[1:]
	c := newFlags("recipe " + sub)
	file := c.fs.String("file", "", "run: a YAML file instead of a saved recipe (try it before add)")
	pos := c.parse(args)
	cfg, st, err := c.load()
	if err != nil {
		return err
	}
	dir := recipe.Dir(cfg.Home)
	switch sub {
	case "list":
		project := ""
		if len(pos) > 0 {
			project = pos[0]
		}
		rs, errs := recipe.List(dir, project)
		for _, r := range rs {
			var ps []string
			for _, p := range r.Params {
				ps = append(ps, p.Name)
			}
			fmt.Printf("%-28s %-10s (%s)  %s\n", r.Name, r.Project, strings.Join(ps, ", "), oneLineText(r.Description, 90))
		}
		for _, e := range errs {
			fmt.Println("lỗi:", e)
		}
		if len(rs) == 0 && len(errs) == 0 {
			fmt.Println("(chưa có quy trình nào: taskpilot reflect, hoặc viết tay rồi taskpilot recipe add FILE)")
		}
		ps, _ := st.RecipeProposals(project, "new")
		if len(ps) > 0 {
			fmt.Printf("\n%d đề xuất đang chờ duyệt: taskpilot reflect list\n", len(ps))
		}
		return nil
	case "show":
		if len(pos) != 1 {
			return errors.New("usage: taskpilot recipe show NAME")
		}
		r, err := recipe.Load(dir, pos[0])
		if err != nil {
			return err
		}
		fmt.Print(recipe.Marshal(r))
		return nil
	case "run":
		var r *recipe.Recipe
		if *file != "" {
			b, err := os.ReadFile(*file)
			if err != nil {
				return err
			}
			if r, err = recipe.Parse(b); err != nil {
				return err
			}
		} else {
			if len(pos) < 1 {
				return errors.New("usage: taskpilot recipe run NAME [param=value …]  |  run -file FILE [param=value …]")
			}
			if r, err = recipe.Load(dir, pos[0]); err != nil {
				return err
			}
			pos = pos[1:]
		}
		vals := map[string]string{}
		for _, kv := range pos {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return fmt.Errorf("%q: viết dạng param=value", kv)
			}
			vals[k] = v
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		out, err := runRecipe(ctx, cfg, st, r, vals)
		if err != nil {
			return err
		}
		fmt.Print(out)
		return nil
	case "check", "add":
		if len(pos) != 1 {
			return fmt.Errorf("usage: taskpilot recipe %s FILE", sub)
		}
		b, err := os.ReadFile(pos[0])
		if err != nil {
			return err
		}
		r, err := recipe.Parse(b)
		if err != nil {
			return err
		}
		if sub == "check" {
			if err := recipe.Validate(r, knownProject(cfg)); err != nil {
				return err
			}
			fmt.Println("hợp lệ:", r.Name)
			return nil
		}
		if err := recipe.Save(dir, r, knownProject(cfg)); err != nil {
			return err
		}
		fmt.Println("đã lưu", recipe.Path(dir, r.Name))
		return recipe.SyncSkills(cfg.Home)
	case "rm":
		if len(pos) != 1 {
			return errors.New("usage: taskpilot recipe rm NAME")
		}
		if err := recipe.Remove(dir, pos[0]); err != nil {
			return err
		}
		fmt.Println("đã xoá", pos[0])
		return recipe.SyncSkills(cfg.Home)
	}
	fmt.Print(recipeUsage)
	return fmt.Errorf("unknown recipe command %q", sub)
}

func cmdReflect(args []string) error {
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	c := newFlags("reflect")
	project := c.fs.String("project", "", "project (default: every project with tasks)")
	days := c.fs.Int("days", 0, "read the tasks of the last N days (default: since the last reflection, at most 30 days)")
	dry := c.fs.Bool("dry", false, "print what Claude would read, without calling it")
	pos := c.parse(args)
	cfg, st, err := c.load()
	if err != nil {
		return err
	}
	id := func() (int64, error) {
		if len(pos) != 1 {
			return 0, fmt.Errorf("usage: taskpilot reflect %s ID", sub)
		}
		return strconv.ParseInt(strings.TrimPrefix(pos[0], "#"), 10, 64)
	}
	switch sub {
	case "list":
		p := ""
		if len(pos) > 0 {
			p = pos[0]
		}
		ps, err := st.RecipeProposals(p, "new")
		if err != nil {
			return err
		}
		if len(ps) == 0 {
			fmt.Println("không có đề xuất nào đang chờ")
		}
		for _, x := range ps {
			fmt.Printf("#%-4d %-10s %-28s %s\n", x.ID, x.Project, x.Name, oneLineText(x.Reason, 90))
		}
		return nil
	case "show":
		n, err := id()
		if err != nil {
			return err
		}
		p, err := st.RecipeProposal(n)
		if err != nil {
			return err
		}
		fmt.Printf("#%d %s (%s, %s)\n", p.ID, p.Name, p.Project, p.Status)
		if p.Replaces != "" {
			fmt.Println("thay cho:", p.Replaces)
		}
		fmt.Println("vì sao:", p.Reason)
		for _, e := range p.Evidence {
			fmt.Println("  •", e)
		}
		fmt.Print("\n" + p.YAML)
		return nil
	case "accept":
		n, err := id()
		if err != nil {
			return err
		}
		r, err := recipe.Accept(st, n, knownProject(cfg))
		if err != nil {
			return err
		}
		fmt.Println("đã lưu quy trình", r.Name, "→", recipe.Path(recipe.Dir(cfg.Home), r.Name))
		return nil
	case "reject":
		n, err := id()
		if err != nil {
			return err
		}
		if err := recipe.Reject(st, n); err != nil {
			return err
		}
		fmt.Println("đã bỏ đề xuất", n)
		return nil
	case "":
	default:
		fmt.Print(reflectUsage)
		return fmt.Errorf("unknown reflect command %q", sub)
	}
	projects := []string{*project}
	if *project == "" {
		projects = cfg.ProjectNames()
	}
	var since time.Time
	if *days > 0 {
		since = time.Now().AddDate(0, 0, -*days)
	}
	rf := &recipe.Reflector{Config: cfg, Store: st}
	for _, p := range projects {
		if *dry {
			from := since
			if from.IsZero() {
				from = time.Now().Add(-recipe.DefaultWindow)
				if last, _ := st.LastReflect(p); last != nil && last.At.After(from) {
					from = last.At
				}
			}
			d, n, err := rf.Digest(p, from)
			if err != nil {
				return err
			}
			fmt.Printf("===== %s: %d task, %d ký tự =====\n%s\n", p, n, len(d), d)
			continue
		}
		fmt.Printf("%s: đang đọc các task…\n", p)
		out, err := rf.Reflect(context.Background(), p, since)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		fmt.Print(reflectSummary(p, out))
	}
	return nil
}

// reflectSummary describes the outcome of a reflection.
func reflectSummary(project string, out *recipe.Outcome) string {
	var b strings.Builder
	if out.Tasks == 0 {
		fmt.Fprintf(&b, "%s: không có task mới từ %s\n", project, out.Since.Local().Format("02/01 15:04"))
		return b.String()
	}
	fmt.Fprintf(&b, "%s: đọc %d task từ %s · $%.2f · %d đề xuất\n", project, out.Tasks, out.Since.Local().Format("02/01"), out.CostUSD, len(out.Proposals))
	for _, p := range out.Proposals {
		fmt.Fprintf(&b, "  #%d %s: %s\n", p.ID, p.Name, oneLineText(p.Reason, 100))
	}
	for _, d := range out.Dropped {
		fmt.Fprintf(&b, "  (bỏ đề xuất không hợp lệ: %s)\n", d)
	}
	if len(out.Proposals) > 0 {
		b.WriteString("  xem: taskpilot reflect show ID · chấp nhận: taskpilot reflect accept ID\n")
	}
	return b.String()
}

func oneLineText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
