package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/buonnguwaaa/taskpilot/internal/argocd"
	"github.com/buonnguwaaa/taskpilot/internal/bitbucket"
	"github.com/buonnguwaaa/taskpilot/internal/claudeai"
	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/daemon"
	"github.com/buonnguwaaa/taskpilot/internal/db"
	"github.com/buonnguwaaa/taskpilot/internal/hasakiwork"
	"github.com/buonnguwaaa/taskpilot/internal/herdr"
	"github.com/buonnguwaaa/taskpilot/internal/intake"
	"github.com/buonnguwaaa/taskpilot/internal/logring"
	"github.com/buonnguwaaa/taskpilot/internal/source"
	"github.com/buonnguwaaa/taskpilot/internal/splgen"
	"github.com/buonnguwaaa/taskpilot/internal/splunk"
	"github.com/buonnguwaaa/taskpilot/internal/tui"
	"github.com/buonnguwaaa/taskpilot/internal/worklog"
)

// cmdTUI runs the interactive console. Unless another process already holds
// the scheduler lock (or -no-daemon is given), the daemon runs inside it.
func cmdTUI(args []string) error {
	c := newFlags("tui")
	noDaemon := c.fs.Bool("no-daemon", false, "do not start a scheduler")
	embedded := c.fs.Bool("embedded-daemon", false, "run the scheduler inside the TUI even under herdr")
	c.fs.Parse(args)
	cfg, st, err := c.load()
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	ctx := context.Background()
	d, err := db.Open(ctx, cfg.Home)
	if err != nil {
		return err
	}
	defer d.Close()
	auth := hasakiwork.NewAuth(d)
	auth.TokenURL, auth.RefreshBefore = cfg.HasakiWork.TokenURL, cfg.HasakiWork.RefreshBefore
	hw := hasakiwork.NewClient(auth)
	hw.BaseURL = cfg.HasakiWork.APIURL
	inbox, err := source.NewMock(cfg.Source.Dir)
	if err != nil {
		return err
	}

	deps := tui.Deps{
		Config: cfg, Store: st, HW: hw, Inbox: inbox, Exe: exe,
		Draft: (&intake.Drafter{Config: cfg}).Draft,
		ClaudeArtifact: func(ctx context.Context, link, dir string) (*claudeai.Artifact, error) {
			f, err := claudeFetcher(ctx, cfg, "tui")
			if err != nil {
				return nil, err
			}
			return f.Fetch(ctx, link, dir)
		},
		WorkLog: (&worklog.Drafter{Config: cfg}).Draft,
		Assess:  (&worklog.Drafter{Config: cfg}).Assess,
		SPLGen: func(ctx context.Context, lc splgen.Context, q string) (*splgen.Result, error) {
			return (&splgen.Generator{Config: cfg}).Generate(ctx, lc, q)
		},
		Banner: func() (string, bool) {
			s, err := hasakiwork.ReadStatus(ctx, d)
			if err != nil {
				return "", false
			}
			now := time.Now()
			if !s.HasToken && !s.HasCookie {
				return "Hasaki Work: chưa đăng nhập (tab 4, phím l)", true
			}
			warn := s.Remaining(now) < cfg.HasakiWork.RefreshBefore && !s.HasCookie || s.Remaining(now) <= 0 ||
				s.CookieExpires != nil && s.CookieExpires.Sub(now) < 24*time.Hour
			return s.Summary(now), warn
		},
		AuthStatus: func() []string {
			return append(authLines(ctx, d), splunkStatus(ctx, cfg, d), argocdStatus(ctx, cfg, d), vaultStatus(ctx, cfg, d), kafkaStatus(ctx, cfg, d), jenkinsStatus(ctx, cfg, d), claudeStatus(ctx, d))
		},
		Splunk: func(ctx context.Context) (*splunk.Client, error) { return splunkClient(ctx, cfg, "operator:tui") },
		ArgoCD: func(ctx context.Context) (*argocd.Client, error) { return argocdClient(ctx, cfg, "operator:tui") },
	}

	var hc *herdr.Client
	if herdr.Inside() {
		hc, _ = herdr.New()
	}
	deps.Herdr = hc

	// Scheduler: under herdr it lives in its own persistent herdr tab, so
	// quitting the TUI never stops tasks; otherwise it runs inside the TUI.
	var stop context.CancelFunc
	done := make(chan struct{})
	close(done)
	if !*noDaemon && len(cfg.Projects) > 0 && hc != nil && !*embedded {
		if err := startDaemonInHerdr(ctx, hc, cfg, exe); err != nil {
			fmt.Fprintf(os.Stderr, "taskpilot: không mở được daemon trong tab herdr (%v); chạy daemon bên trong TUI\n", err)
		}
	}
	if !*noDaemon && len(cfg.Projects) > 0 {
		unlock, err := daemon.Lock(cfg.Home)
		switch {
		case errors.Is(err, daemon.ErrLocked):
			deps.Daemon.External = true
			deps.Daemon.InHerdr = hc != nil
		case err != nil:
			return err
		default:
			defer unlock()
			ring := logring.New(500)
			logFile, err := os.OpenFile(filepath.Join(cfg.Home, "daemon.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			defer logFile.Close()
			dm, err := newDaemon(cfg, st, exe, io.MultiWriter(logFile, ring))
			if err != nil {
				return err
			}
			var dctx context.Context
			dctx, stop = context.WithCancel(ctx)
			done = make(chan struct{})
			go func() {
				defer close(done)
				if err := dm.Run(dctx); err != nil {
					dm.Log.Printf("daemon stopped: %v", err)
				}
			}()
			deps.Daemon = tui.DaemonInfo{Embedded: true, InFlight: dm.InFlight, Logs: ring.Lines}
		}
	}

	err = tui.Run(deps)
	if stop != nil {
		if n := deps.Daemon.InFlight(); n > 0 {
			fmt.Fprintf(os.Stderr, "taskpilot: đang dừng %d task (chúng sẽ ở trạng thái needs_attention)…\n", n)
		}
		stop()
		<-done
	}
	return err
}

// authLines describes the stored credentials for the status page.
func authLines(ctx context.Context, d *db.DB) []string {
	var out []string
	if s, err := hasakiwork.ReadStatus(ctx, d); err == nil {
		out = append(out, s.Summary(time.Now()))
		if s.TokenExpiresAt != nil {
			out = append(out, "  JWT hết hạn:       "+s.TokenExpiresAt.Local().Format("2006-01-02 15:04"))
		}
		if s.CookieExpires != nil {
			out = append(out, "  phiên SSO hết hạn: "+s.CookieExpires.Local().Format("2006-01-02 15:04"))
		}
	}
	if m, err := d.Meta(ctx, bitbucket.SecretToken); err == nil {
		exp := "không đặt hạn"
		if m.ExpiresAt != nil {
			exp = "hết hạn " + m.ExpiresAt.Format("2006-01-02")
		}
		out = append(out, "Bitbucket: có API token ("+exp+") — PR được tạo tự động")
	} else {
		out = append(out, "Bitbucket: không có API token — push bằng SSH key, tạo PR từ TUI (phím p)")
	}
	return out
}

// splunkStatus is a one-line description of the Splunk setup (never values).
func splunkStatus(ctx context.Context, cfg *config.Config, d *db.DB) string {
	if cfg.Splunk.URL == "" {
		return "Splunk: chưa cấu hình (splunk.url trong config)"
	}
	if m, err := d.Meta(ctx, splunk.SecretToken); err == nil {
		switch {
		case m.ExpiresAt == nil:
			return "Splunk: có API token, không có hạn (" + cfg.Splunk.URL + ")"
		case m.ExpiresAt.Before(time.Now()):
			return "Splunk: API token ĐÃ HẾT HẠN — tạo token mới, phím s hoặc `taskpilot auth splunk -token`"
		case time.Until(*m.ExpiresAt) < 7*24*time.Hour:
			return fmt.Sprintf("Splunk: API token sắp hết hạn (%s, còn %d ngày)", m.ExpiresAt.Local().Format("2006-01-02"), int(time.Until(*m.ExpiresAt).Hours()/24))
		default:
			return "Splunk: có API token, hết hạn " + m.ExpiresAt.Local().Format("2006-01-02")
		}
	}
	if m, err := d.Meta(ctx, splunk.SecretCookie); err == nil {
		s := "Splunk: đăng nhập bằng phiên web (" + cfg.Splunk.URL + ")"
		if m.ExpiresAt != nil {
			if m.ExpiresAt.Before(time.Now()) {
				return s + " — ĐÃ HẾT HẠN, phím s để đăng nhập lại"
			}
			s += ", hết hạn " + m.ExpiresAt.Local().Format("2006-01-02 15:04")
		}
		return s
	}
	return "Splunk: chưa đăng nhập — phím s (tab Status) hoặc `taskpilot auth splunk`"
}

// startDaemonInHerdr opens a herdr tab running `taskpilot daemon` unless a
// scheduler already holds the lock, then waits until the new one does.
func startDaemonInHerdr(ctx context.Context, hc *herdr.Client, cfg *config.Config, exe string) error {
	release, err := daemon.Lock(cfg.Home)
	if errors.Is(err, daemon.ErrLocked) {
		return nil // a daemon already runs (another tab, another TUI)
	}
	if err != nil {
		return err
	}
	release()
	_, pane, err := hc.CreateTab(ctx, "taskpilot daemon", cfg.Home)
	if err != nil {
		return err
	}
	if err := hc.Run(ctx, pane, herdr.Command(exe, "daemon", "-config", cfg.Path)); err != nil {
		return err
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if release, err := daemon.Lock(cfg.Home); errors.Is(err, daemon.ErrLocked) {
			return nil
		} else if err == nil {
			release()
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("daemon in herdr pane %s did not start within 15s (look at that tab)", pane)
}
