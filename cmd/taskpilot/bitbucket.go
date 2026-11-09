package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/buonnguwaaa/taskpilot/internal/bitbucket"
	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/db"
)

// bitbucketClient builds a client from the credentials stored in the database.
func bitbucketClient(ctx context.Context, cfg *config.Config, consumer string) (*bitbucket.Client, error) {
	d, err := db.Open(ctx, cfg.Home)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	if _, err := d.Meta(ctx, bitbucket.SecretToken); errors.Is(err, db.ErrNoSecret) {
		return nil, bitbucket.ErrNoCredentials
	}
	email, _, err := d.Resolve(ctx, bitbucket.SecretEmail, consumer)
	if err != nil {
		return nil, fmt.Errorf("bitbucket email missing (run `taskpilot auth bitbucket`): %w", err)
	}
	token, meta, err := d.Resolve(ctx, bitbucket.SecretToken, consumer)
	if err != nil {
		return nil, err
	}
	if meta.ExpiresAt != nil && meta.ExpiresAt.Before(time.Now()) {
		return nil, fmt.Errorf("the Bitbucket API token expired on %s; create a new one and run `taskpilot auth bitbucket`", meta.ExpiresAt.Format("2006-01-02"))
	}
	return bitbucket.New(email, token), nil
}

// repoRefs maps "project/repo" to the Bitbucket repository of each configured repo.
func repoRefs(cfg *config.Config) (names []string, refs map[string]bitbucket.RepoRef) {
	refs = map[string]bitbucket.RepoRef{}
	for _, pname := range cfg.ProjectNames() {
		p := cfg.Projects[pname]
		for _, rname := range p.RepoNames() {
			r := p.Repos[rname]
			if r.Remote == "-" {
				continue
			}
			out, err := exec.Command("git", "-C", r.Path, "config", "--get", "remote."+r.Remote+".url").Output()
			if err != nil {
				continue
			}
			if ref, err := bitbucket.ParseRemote(string(out)); err == nil {
				key := pname + "/" + rname
				names = append(names, key)
				refs[key] = ref
			}
		}
	}
	return names, refs
}

func authBitbucket(ctx context.Context, cfg *config.Config, d *db.DB, expires string) error {
	names, refs := repoRefs(cfg)
	if len(names) == 0 {
		return errors.New("no configured repo has a Bitbucket remote to test the credentials against")
	}
	fmt.Fprintln(os.Stderr, "Tạo API token: id.atlassian.com → Security → API tokens → Create API token with scopes → Bitbucket,")
	fmt.Fprintln(os.Stderr, "chọn scope: read:repository:bitbucket, read:pullrequest:bitbucket, write:pullrequest:bitbucket.")
	email, err := readLine("Email Atlassian (đăng nhập Bitbucket): ")
	if err != nil {
		return err
	}
	token, err := readSecret("API token: ")
	if err != nil {
		return err
	}
	if email == "" || token == "" {
		return errors.New("email and token are required")
	}
	ref := refs[names[0]]
	if err := bitbucket.New(email, token).CheckAccess(ctx, ref); err != nil {
		return fmt.Errorf("check against %s: %w", ref, err)
	}
	var exp *time.Time
	if expires != "" {
		t, err := time.Parse("2006-01-02", expires)
		if err != nil {
			return fmt.Errorf("-expires: want YYYY-MM-DD: %w", err)
		}
		exp = &t
	}
	if err := d.SetPlain(ctx, bitbucket.SecretEmail, email, "Atlassian account email for the Bitbucket API", nil, "operator"); err != nil {
		return err
	}
	if err := d.SetPlain(ctx, bitbucket.SecretToken, token, "Bitbucket API token (PR scopes)", exp, "operator"); err != nil {
		return err
	}
	fmt.Printf("đã lưu thông tin Bitbucket (kiểm tra truy cập %s: OK)\n", ref)
	return nil
}

func readLine(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	var s string
	_, err := fmt.Fscanln(os.Stdin, &s)
	return strings.TrimSpace(s), err
}

func cmdBB(args []string) error {
	if len(args) == 0 || args[0] != "pr-targets" {
		fmt.Print("taskpilot bb pr-targets   where recent merged PRs of each repo went\n")
		return nil
	}
	c := newFlags("bb pr-targets")
	c.fs.Parse(args[1:])
	cfg, err := config.Load(*c.config)
	if err != nil {
		return err
	}
	ctx := context.Background()
	bb, err := bitbucketClient(ctx, cfg, "operator:bb")
	if err != nil {
		return err
	}
	names, refs := repoRefs(cfg)
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "REPO\tpr_flow\tFEATURE PRs →\tPROMOTIONS")
	for _, key := range names {
		pname, rname, _ := strings.Cut(key, "/")
		prs, err := bb.RecentMerged(ctx, refs[key], 50)
		if err != nil {
			fmt.Fprintf(w, "%s\t%s\terror: %v\t\n", key, flowTargets(cfg.Projects[pname].Repos[rname]), err)
			continue
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", key, flowTargets(cfg.Projects[pname].Repos[rname]),
			countList(bitbucket.FeatureDestinations(prs)), countList(bitbucket.Promotions(prs)))
	}
	return w.Flush()
}

func countList(m map[string]int) string {
	type kv struct {
		k string
		v int
	}
	var list []kv
	for k, v := range m {
		list = append(list, kv{k, v})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].v > list[j].v || list[i].v == list[j].v && list[i].k < list[j].k })
	var parts []string
	for _, x := range list {
		parts = append(parts, fmt.Sprintf("%s ×%d", x.k, x.v))
	}
	return strings.Join(parts, ", ")
}

func flowTargets(r config.Repo) string {
	var t []string
	for _, st := range r.PRFlow {
		t = append(t, st.Target)
	}
	return strings.Join(t, " → ")
}
