package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/buonnguwaaa/taskpilot/internal/argocd"
	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/db"
	"github.com/buonnguwaaa/taskpilot/internal/hasakiwork"
	"github.com/buonnguwaaa/taskpilot/internal/splunk"
)

// argocdClient builds a client from the stored token.
func argocdClient(ctx context.Context, cfg *config.Config, consumer string) (*argocd.Client, error) {
	if cfg.ArgoCD.URL == "" {
		return nil, errors.New("argocd.url is not set in the taskpilot config")
	}
	tok, meta, err := storedSecret(ctx, cfg, argocd.SecretToken, consumer, argocd.ErrNoCredentials)
	if err != nil {
		return nil, err
	}
	if meta.ExpiresAt != nil && meta.ExpiresAt.Before(time.Now()) {
		return nil, fmt.Errorf("%w (expired %s)", argocd.ErrUnauthorized, meta.ExpiresAt.Local().Format("2006-01-02 15:04"))
	}
	return &argocd.Client{BaseURL: cfg.ArgoCD.URL, Token: tok}, nil
}

// projectApps returns the Argo CD apps of a taskpilot project.
func projectApps(cfg *config.Config, name string, apps []argocd.App) []argocd.App {
	p := cfg.Projects[name]
	return argocd.Match(apps, p.ArgoCD.Apps, p.ArgoCD.Projects, p.RepoNames(), p.ArgoCD.MatchesRepos(), cfg.AllRepoNames())
}

// authArgoCD stores an Argo CD token: from the dedicated Chrome profile after
// login (cookie argocd.token), or pasted with -token.
func authArgoCD(ctx context.Context, cfg *config.Config, d *db.DB, paste bool, timeout time.Duration) error {
	if cfg.ArgoCD.URL == "" {
		return errors.New("set argocd.url in the taskpilot config first")
	}
	var tok string
	if paste {
		v, err := readSecret("Argo CD token (giá trị cookie argocd.token hoặc token của account): ")
		if err != nil {
			return err
		}
		tok = strings.TrimSpace(strings.TrimPrefix(v, "Bearer "))
	} else {
		hw := cfg.HasakiWork
		if hw.ChromeProfile == "" {
			return errors.New("set hasaki_work.chrome_profile (the dedicated Chrome profile) or use -token")
		}
		u, err := url.Parse(cfg.ArgoCD.URL)
		if err != nil {
			return err
		}
		ch := &hasakiwork.Chrome{Exe: hw.ChromeExe, ProfileDir: hw.ChromeProfile, Port: hw.DebugPort}
		sctx, stop := signal.NotifyContext(ctx, os.Interrupt)
		defer stop()
		if err := ch.Open(sctx, cfg.ArgoCD.URL+cfg.ArgoCD.LoginPath); err != nil {
			return err
		}
		defer closeChrome(ch)
		fmt.Println("Chrome (profile riêng của taskpilot) đã mở Argo CD. Đăng nhập trên cửa sổ đó…")
		wctx, cancel := context.WithTimeout(sctx, timeout)
		defer cancel()
		cookies, err := ch.WaitForCookies(wctx, u.Hostname(), func(cs []hasakiwork.CDPCookie) bool {
			for _, c := range cs {
				if strings.HasPrefix(c.Name, argocd.CookieName) && c.Value != "" {
					return true
				}
			}
			return false
		})
		if err != nil {
			return err
		}
		// Large tokens are split into argocd.token, argocd.token-1, … (in order).
		var parts []string
		for _, name := range []string{argocd.CookieName, argocd.CookieName + "-1", argocd.CookieName + "-2", argocd.CookieName + "-3"} {
			for _, c := range cookies {
				if c.Name == name {
					parts = append(parts, c.Value)
				}
			}
		}
		tok = strings.Join(parts, "")
	}
	if tok == "" {
		return errors.New("no token")
	}
	user, err := (&argocd.Client{BaseURL: cfg.ArgoCD.URL, Token: tok}).UserInfo(ctx)
	if err != nil {
		return fmt.Errorf("check against %s: %w", cfg.ArgoCD.URL, err)
	}
	exp, _ := splunk.TokenExpiry(tok) // the same JWT exp reader
	if err := d.SetPlain(ctx, argocd.SecretToken, tok, "Argo CD QC session token", exp, "operator"); err != nil {
		return err
	}
	when := "không có hạn"
	if exp != nil {
		when = "hết hạn " + exp.Local().Format("2006-01-02 15:04")
	}
	fmt.Printf("đã lưu token Argo CD (đăng nhập là %s, %s)\n", user, when)
	return nil
}

// argocdStatus is a one-line description for status pages (never the token).
func argocdStatus(ctx context.Context, cfg *config.Config, d *db.DB) string {
	if cfg.ArgoCD.URL == "" {
		return "Argo CD: chưa cấu hình (argocd.url)"
	}
	m, err := d.Meta(ctx, argocd.SecretToken)
	if err != nil {
		return "Argo CD " + cfg.ArgoCD.Env + ": chưa đăng nhập — phím a (tab Status) hoặc `taskpilot auth argocd`"
	}
	switch {
	case m.ExpiresAt == nil:
		return "Argo CD " + cfg.ArgoCD.Env + ": có token, không có hạn"
	case m.ExpiresAt.Before(time.Now()):
		return "Argo CD " + cfg.ArgoCD.Env + ": token ĐÃ HẾT HẠN — phím a để đăng nhập lại"
	default:
		return fmt.Sprintf("Argo CD %s: có token, hết hạn %s", cfg.ArgoCD.Env, m.ExpiresAt.Local().Format("2006-01-02 15:04"))
	}
}

func cmdArgo(args []string) error {
	if len(args) == 0 || (args[0] != "apps" && args[0] != "logs") {
		fmt.Print("taskpilot argo apps [-project P]          applications and the project each maps to\n" +
			"taskpilot argo logs APP [-filter S] [-tail N]   latest pod logs of an application\n")
		return nil
	}
	sub := args[0]
	c := newFlags("argo " + sub)
	project := c.fs.String("project", "", "only this taskpilot project")
	filter := c.fs.String("filter", "", "logs: only lines containing this text")
	tail := c.fs.Int("tail", 100, "logs: number of lines")
	rest := args[1:]
	appName := ""
	if sub == "logs" && len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		appName, rest = rest[0], rest[1:]
	}
	c.fs.Parse(rest)
	cfg, err := config.Load(*c.config)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cl, err := argocdClient(ctx, cfg, "operator:cli")
	if err != nil {
		return err
	}
	apps, err := cl.Apps(ctx)
	if err != nil {
		return err
	}
	owner := map[string]string{}
	for _, p := range cfg.ProjectNames() {
		for _, a := range projectApps(cfg, p, apps) {
			owner[a.Name] = p
		}
	}
	if sub == "apps" {
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "APP\tTASKPILOT PROJECT\tARGO PROJECT\tHEALTH\tSYNC\tNAMESPACE")
		for _, a := range apps {
			if *project != "" && owner[a.Name] != *project {
				continue
			}
			o := owner[a.Name]
			if o == "" {
				o = "-"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", a.Name, o, a.Project, a.Health, a.Sync, a.Namespace)
		}
		return w.Flush()
	}
	for _, a := range apps {
		if a.Name == appName {
			lines, err := cl.Logs(ctx, a, argocd.LogOptions{TailLines: *tail, Filter: *filter})
			if err != nil {
				return err
			}
			for _, l := range lines {
				fmt.Printf("%s %s %s\n", l.Time, l.Pod, l.Content)
			}
			return nil
		}
	}
	return fmt.Errorf("no application %q (taskpilot argo apps lists them)", appName)
}
