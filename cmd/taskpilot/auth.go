package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/buonnguwaaa/taskpilot/internal/bitbucket"
	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/db"
	"github.com/buonnguwaaa/taskpilot/internal/hasakiwork"
)

const authUsage = `taskpilot auth <command>

  status        show token / session expiry (never prints values)
  login         open the dedicated Chrome profile, wait for SSO login, store the session (option A)
  set-token     paste a JWT (read from stdin, not echoed) (option C)
  set-cookie    paste a work.hasaki.vn Cookie header (read from stdin, not echoed)
  refresh       get a fresh JWT from the stored session cookie now
  bitbucket     store the Bitbucket email + API token used to open PRs [-expires YYYY-MM-DD]
  argocd        log in to Argo CD QC with the dedicated Chrome profile [-token to paste instead]
  set-db P      store the MySQL DSN of project P's QC database (read from stdin, not echoed)
  splunk        log in to Splunk with the dedicated Chrome profile [-create: then make a personal API token, -expires-in +90d]
                [-token | -cookie to paste instead]
`

func openAuth(c common) (*config.Config, *db.DB, *hasakiwork.Auth, error) {
	cfg, err := config.Load(*c.config)
	if err != nil {
		return nil, nil, nil, err
	}
	d, err := db.Open(context.Background(), cfg.Home)
	if err != nil {
		return nil, nil, nil, err
	}
	a := hasakiwork.NewAuth(d)
	a.TokenURL = cfg.HasakiWork.TokenURL
	a.RefreshBefore = cfg.HasakiWork.RefreshBefore
	return cfg, d, a, nil
}

func cmdAuth(args []string) error {
	if len(args) == 0 {
		fmt.Print(authUsage)
		return nil
	}
	sub, args := args[0], args[1:]
	c := newFlags("auth " + sub)
	timeout := c.fs.Duration("timeout", 10*time.Minute, "login: how long to wait for the SSO login")
	keepOpen := c.fs.Bool("keep-open", false, "login: leave Chrome (and its debug port) open afterwards")
	expires := c.fs.String("expires", "", "bitbucket: token expiry date YYYY-MM-DD (shown in status)")
	useToken := c.fs.Bool("token", false, "splunk: paste an API token instead of logging in with Chrome")
	useCookie := c.fs.Bool("cookie", false, "splunk: paste a Splunk Web Cookie header")
	create := c.fs.Bool("create", false, "splunk: after logging in with Chrome, create a personal API token and use it")
	ttl := c.fs.String("expires-in", "+90d", "splunk -create: token lifetime as a Splunk time (+90d, +180d) or never")
	c.fs.Parse(args)
	cfg, d, a, err := openAuth(c)
	if err != nil {
		return err
	}
	defer d.Close()
	ctx := context.Background()

	switch sub {
	case "status":
		st, err := hasakiwork.ReadStatus(ctx, d)
		if err != nil {
			return err
		}
		now := time.Now()
		fmt.Println(st.Summary(now))
		if st.TokenExpiresAt != nil {
			fmt.Printf("  JWT hết hạn:        %s\n", st.TokenExpiresAt.Local().Format("2006-01-02 15:04"))
		}
		if st.CookieExpires != nil {
			fmt.Printf("  phiên SSO hết hạn:  %s\n", st.CookieExpires.Local().Format("2006-01-02 15:04"))
		}
		if m, err := d.Meta(ctx, bitbucket.SecretToken); err == nil {
			exp := "không đặt hạn"
			if m.ExpiresAt != nil {
				exp = "hết hạn " + m.ExpiresAt.Format("2006-01-02")
			}
			fmt.Printf("Bitbucket: đã có API token (%s)\n", exp)
		} else {
			fmt.Println("Bitbucket: không có API token — agent push bằng SSH key của máy, bạn tạo PR từ TUI (phím p); `taskpilot auth bitbucket` để tự động hoàn toàn")
		}
		fmt.Println(splunkStatus(ctx, cfg, d))
		fmt.Println(argocdStatus(ctx, cfg, d))
		fmt.Printf("  database:           %s\n", d.Path)
		return nil

	case "set-token":
		v, err := readSecret("Dán JWT của Hasaki Work rồi Enter: ")
		if err != nil {
			return err
		}
		exp, err := a.SetToken(ctx, v, "operator")
		if err != nil {
			return err
		}
		fmt.Printf("đã lưu token, hết hạn %s\n", exp.Local().Format("2006-01-02 15:04"))
		return nil

	case "set-db":
		if c.fs.NArg() != 1 {
			return fmt.Errorf("usage: taskpilot auth set-db PROJECT")
		}
		return authSetDB(ctx, cfg, d, c.fs.Arg(0))

	case "set-cookie":
		v, err := readSecret("Dán Cookie header của work.hasaki.vn rồi Enter: ")
		if err != nil {
			return err
		}
		if err := a.SetCookie(ctx, v, nil, "operator"); err != nil {
			return err
		}
		fmt.Printf("đã lưu cookie (%s)\n", strings.Join(hasakiwork.CookieNames(v), ", "))
		return nil

	case "refresh":
		tok, err := a.Refresh(ctx)
		if err != nil {
			return err
		}
		cl, _ := hasakiwork.ParseClaims(tok)
		fmt.Printf("token hết hạn %s\n", time.Unix(cl.Exp, 0).Local().Format("2006-01-02 15:04"))
		return nil

	case "login":
		return authLogin(ctx, cfg, a, *timeout, *keepOpen)

	case "bitbucket":
		return authBitbucket(ctx, cfg, d, *expires)

	case "argocd":
		return authArgoCD(ctx, cfg, d, *useToken, *timeout)

	case "splunk":
		return authSplunk(ctx, cfg, d, *useToken, *useCookie, *create, *ttl, *timeout)
	}
	fmt.Print(authUsage)
	return fmt.Errorf("unknown auth command %q", sub)
}

func authLogin(ctx context.Context, cfg *config.Config, a *hasakiwork.Auth, timeout time.Duration, keepOpen bool) error {
	hw := cfg.HasakiWork
	if hw.ChromeProfile == "" {
		return errors.New("set hasaki_work.chrome_profile in the config (for Windows Chrome use a Windows path, e.g. C:\\Users\\<you>\\AppData\\Local\\taskpilot-chrome)")
	}
	ch := &hasakiwork.Chrome{Exe: hw.ChromeExe, ProfileDir: hw.ChromeProfile, Port: hw.DebugPort}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	if err := ch.Launch(ctx, "https://work.hasaki.vn/tasks"); err != nil {
		return err
	}
	fmt.Println("Chrome (profile riêng của taskpilot) đã mở. Đăng nhập SSO trên cửa sổ đó nếu được hỏi…")
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	header, cookies, err := ch.WaitForSession(wctx, "work.hasaki.vn")
	if err != nil {
		return err
	}
	var sessionExp *time.Time
	var jwt string
	for _, ck := range cookies {
		if ck.Name == hasakiwork.SessionCookieName {
			jwt = ck.Value
			if ck.Expires > 0 {
				t := time.Unix(int64(ck.Expires), 0).UTC()
				sessionExp = &t
			}
		}
	}
	if err := a.SetCookie(ctx, header, sessionExp, "operator"); err != nil {
		return err
	}
	if _, err := hasakiwork.ParseClaims(jwt); err == nil {
		if _, err := a.SetToken(ctx, jwt, "operator"); err != nil {
			return err
		}
	} else if _, err := a.Refresh(ctx); err != nil {
		return fmt.Errorf("stored the session but could not get a JWT: %w", err)
	}
	if !keepOpen {
		if err := ch.Close(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not close Chrome (%v); close it yourself so the debug port goes away\n", err)
		}
	}
	st, err := hasakiwork.ReadStatus(ctx, a.DB)
	if err != nil {
		return err
	}
	fmt.Println(st.Summary(time.Now()))
	return nil
}

// readSecret reads one value without echoing when stdin is a terminal.
func readSecret(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		fmt.Fprint(os.Stderr, prompt)
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		return strings.TrimSpace(string(b)), err
	}
	b, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	return strings.TrimSpace(string(b)), err
}

const hwUsage = `taskpilot hw <command>

  me                     show the logged-in Hasaki Work user
  tasks [-status _00_01] list my tasks (_00_01 open, _05 waiting approval, _02 finished, _04 cancelled)
  show ID                show one task with its description and comment count
  get PATH [k=v ...]     GET a raw API path (read-only, for exploring the API)
  epics                  show the open DEV / Improve / Issue Production EPIC of each project (used by TUI key h)
`

func cmdHW(args []string) error {
	if len(args) == 0 {
		fmt.Print(hwUsage)
		return nil
	}
	sub, args := args[0], args[1:]
	c := newFlags("hw " + sub)
	status := c.fs.String("status", "_00_01", "status filter")
	id := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id, args = args[0], args[1:]
	}
	if sub == "get" { // k=v pairs are the query, not flags
		args, id = nil, strings.Join(args, "&")+"\x00"+id
	}
	c.fs.Parse(args)
	cfg, d, a, err := openAuth(c)
	if err != nil {
		return err
	}
	defer d.Close()
	cl := hasakiwork.NewClient(a)
	cl.BaseURL = cfg.HasakiWork.APIURL
	ctx := context.Background()

	switch sub {
	case "me":
		p, err := cl.Profile(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("%d %s <%s>\n", p.ID, p.Name, p.Email)
		return nil
	case "tasks":
		tasks, err := cl.MyTasks(ctx, *status)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tCODE\tSTATUS\tSCOPE\tDEADLINE\tNAME")
		for _, t := range tasks {
			scope := "-"
			if name, _ := cfg.ProjectByHasakiID(t.ProjectID); name != "" {
				scope = name
			} else if t.Project != nil {
				scope = "(" + t.Project.Name + ")"
			}
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", t.ID, t.Code, hasakiwork.StatusName(t.Status), scope, short(t.DateEnd, 10), short(t.Name, 70))
		}
		return w.Flush()
	case "get":
		q, path, _ := strings.Cut(id, "\x00")
		vals, err := url.ParseQuery(q)
		if err != nil {
			return err
		}
		raw, err := cl.GetRaw(ctx, path, vals)
		if err != nil {
			return err
		}
		fmt.Println(string(raw))
		return nil
	case "epics":
		var mine []hasakiwork.Task
		for _, st := range []string{"_00_01", "_05", "_02"} {
			ts, err := cl.MyTasks(ctx, st)
			if err != nil {
				return err
			}
			mine = append(mine, ts...)
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "PROJECT\tKIND\tEPIC ID\tCODE\tSPRINT\tNAME")
		for _, name := range cfg.ProjectNames() {
			p := cfg.Projects[name]
			if p.HasakiProjectID == 0 {
				continue
			}
			eps, err := cl.CurrentEpics(ctx, p.HasakiProjectID, mine)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			for _, k := range hasakiwork.EpicKinds {
				if e := eps[k]; e != nil {
					fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%d %s\t%s\n", name, k.Label(), e.ID, e.Code, e.MilestoneID, short(e.Milestone, 30), e.Name)
				} else {
					fmt.Fprintf(w, "%s\t%s\t-\t\t\t(không tìm thấy EPIC đang mở)\n", name, k.Label())
				}
			}
		}
		return w.Flush()
	case "show":
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			return fmt.Errorf("usage: taskpilot hw show ID")
		}
		t, err := cl.Task(ctx, n)
		if err != nil {
			return err
		}
		comments, err := cl.Comments(ctx, n)
		if err != nil {
			return err
		}
		proj := ""
		if t.Project != nil {
			proj = t.Project.Name
		}
		fmt.Printf("%s  %s\nproject: %s  status: %s  deadline: %s\n", t.Code, t.Name, proj, hasakiwork.StatusName(t.Status), t.DateEnd)
		if t.CreatedBy != nil {
			fmt.Printf("created by: %s\n", t.CreatedBy.StaffName)
		}
		fmt.Printf("comments: %d\n\n%s\n", len(comments), strings.TrimSpace(t.Note))
		return nil
	}
	fmt.Print(hwUsage)
	return fmt.Errorf("unknown hw command %q", sub)
}

func short(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
