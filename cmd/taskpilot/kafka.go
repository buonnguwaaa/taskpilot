package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/db"
	"github.com/buonnguwaaa/taskpilot/internal/hasakiwork"
	"github.com/buonnguwaaa/taskpilot/internal/kafbat"
)

// kafkaClient returns a Kafbat client with the operator's stored session.
func kafkaClient(ctx context.Context, cfg *config.Config, consumer string) (*kafbat.Client, error) {
	if cfg.Kafka.URL == "" {
		return nil, errors.New("kafka.url is not set in the taskpilot config")
	}
	ck, _, err := storedSecret(ctx, cfg, kafbat.SecretCookie, consumer, kafbat.ErrNoCredentials)
	if err != nil {
		return nil, err
	}
	return &kafbat.Client{BaseURL: cfg.Kafka.URL, Cookie: ck}, nil
}

// kafkaCluster is the configured cluster, or the only one the UI has.
func kafkaCluster(ctx context.Context, cfg *config.Config, cl *kafbat.Client) (string, error) {
	if cfg.Kafka.Cluster != "" {
		return cfg.Kafka.Cluster, nil
	}
	cs, err := cl.Clusters(ctx)
	if err != nil {
		return "", err
	}
	switch len(cs) {
	case 0:
		return "", errors.New("kafka-ui has no cluster")
	case 1:
		return cs[0].Name, nil
	}
	names := make([]string, len(cs))
	for i, c := range cs {
		names[i] = c.Name
	}
	return "", fmt.Errorf("kafka-ui has several clusters (%s): set kafka.cluster in the taskpilot config", strings.Join(names, ", "))
}

// authKafka logs in to the QC Kafka UI with SSO in the dedicated Chrome
// profile (or a pasted Cookie header) and stores the session cookie.
func authKafka(ctx context.Context, cfg *config.Config, d *db.DB, paste bool, timeout time.Duration) error {
	if cfg.Kafka.URL == "" {
		return errors.New("set kafka.url in the taskpilot config first (e.g. https://kafka-ui-qc-v2.hasaki.dev)")
	}
	var cookie string
	if paste {
		v, err := readSecret("Cookie của Kafka UI (cả header Cookie, hoặc chỉ giá trị SESSION): ")
		if err != nil {
			return err
		}
		cookie = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "Cookie:"))
		if cookie != "" && !strings.Contains(cookie, "=") {
			cookie = kafbat.SessionCookieName + "=" + cookie
		}
	} else {
		hw := cfg.HasakiWork
		if hw.ChromeProfile == "" {
			return errors.New("set hasaki_work.chrome_profile (the dedicated Chrome profile) or use -token")
		}
		u, err := url.Parse(cfg.Kafka.URL)
		if err != nil {
			return err
		}
		ch := &hasakiwork.Chrome{Exe: hw.ChromeExe, ProfileDir: hw.ChromeProfile, Port: hw.DebugPort}
		sctx, stop := signal.NotifyContext(ctx, os.Interrupt)
		defer stop()
		if err := ch.Open(sctx, cfg.Kafka.URL+cfg.Kafka.LoginPath); err != nil {
			return err
		}
		defer closeChrome(ch)
		fmt.Println("Chrome (profile riêng của taskpilot) đã mở Kafka UI " + cfg.Kafka.Env + ". Đăng nhập SSO trên cửa sổ đó…")
		wctx, cancel := context.WithTimeout(sctx, timeout)
		defer cancel()
		cookie, err = waitKafkaSession(wctx, ch, cfg.Kafka.URL, u.Hostname())
		if err != nil {
			return err
		}
	}
	if cookie == "" {
		return errors.New("no cookie")
	}
	cl := &kafbat.Client{BaseURL: cfg.Kafka.URL, Cookie: cookie}
	cs, err := cl.Clusters(ctx)
	if err != nil {
		return fmt.Errorf("check against %s: %w", cfg.Kafka.URL, err)
	}
	if err := d.SetPlain(ctx, kafbat.SecretCookie, cookie, "Kafka UI "+cfg.Kafka.Env+" session cookie (read-only use)", nil, "operator"); err != nil {
		return err
	}
	user, _ := cl.User(ctx)
	if user == "" {
		user = "SSO"
	}
	names := make([]string, len(cs))
	for i, c := range cs {
		names[i] = c.Name
	}
	fmt.Printf("đã lưu phiên Kafka UI %s (%s, clusters: %s)\n", cfg.Kafka.Env, user, strings.Join(names, ", "))
	return nil
}

// waitKafkaSession polls the profile's cookies for the Kafka UI host until
// they make a working session. Spring sets SESSION before the login is done
// (to remember the OAuth request), so a cookie alone is not enough: each new
// cookie set is tried against the API.
func waitKafkaSession(ctx context.Context, ch *hasakiwork.Chrome, baseURL, host string) (string, error) {
	tried := map[string]bool{}
	for {
		if cks, err := ch.Cookies(ctx, host); err == nil {
			has := false
			for _, c := range cks {
				if c.Name == kafbat.SessionCookieName && c.Value != "" {
					has = true
				}
			}
			if h := hasakiwork.CookieHeader(cks); has && !tried[h] {
				tried[h] = true
				_, err := (&kafbat.Client{BaseURL: baseURL, Cookie: h}).Clusters(ctx)
				if err == nil {
					return h, nil
				}
				if !errors.Is(err, kafbat.ErrUnauthorized) {
					return "", fmt.Errorf("check against %s: %w", baseURL, err)
				}
			}
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("hết thời gian chờ đăng nhập trên %s", host)
		case <-time.After(2 * time.Second):
		}
	}
}

// kafkaStatus is a one-line description for status pages (never the cookie).
func kafkaStatus(ctx context.Context, cfg *config.Config, d *db.DB) string {
	if cfg.Kafka.URL == "" {
		return "Kafka UI: chưa cấu hình (kafka.url)"
	}
	m, err := d.Meta(ctx, kafbat.SecretCookie)
	if err != nil {
		return "Kafka UI " + cfg.Kafka.Env + ": chưa đăng nhập — phím K (tab Status) hoặc `taskpilot auth kafka`"
	}
	return fmt.Sprintf("Kafka UI %s: có phiên đăng nhập từ %s (hết hạn khi server bỏ phiên — `taskpilot kafka whoami` để kiểm tra)",
		cfg.Kafka.Env, m.UpdatedAt.Local().Format("2006-01-02 15:04"))
}

const kafkaUsage = `taskpilot kafka <command>     (QC Kafka through Kafka UI, read-only)

  whoami                    check the stored session; list clusters
  topics [SEARCH]           topics whose name contains SEARCH
  topic TOPIC               partitions and offsets of a topic
  messages TOPIC [flags]    read messages (newest first by default)
      -limit N  -filter TEXT  -partition P  -from-offset O  -since 2h|2026-11-12T08:00
  groups TOPIC              consumer groups of a topic and their lag
  group ID                  one consumer group, per partition
`

func cmdKafka(args []string) error {
	if len(args) == 0 {
		fmt.Print(kafkaUsage)
		return nil
	}
	sub, args := args[0], args[1:]
	c := newFlags("kafka " + sub)
	limit := c.fs.Int("limit", 20, "messages: how many")
	filter := c.fs.String("filter", "", "messages: only those whose key or value contains this text")
	partition := c.fs.Int("partition", -1, "messages: only this partition")
	fromOffset := c.fs.Int64("from-offset", -1, "messages: read forward from this offset")
	since := c.fs.String("since", "", "messages: read forward from this time (2h, 30m, or RFC 3339 / 2006-01-02T15:04 local)")
	pos := c.parse(args)
	cfg, err := config.Load(*c.config)
	if err != nil {
		return err
	}
	ctx := context.Background()
	cl, err := kafkaClient(ctx, cfg, "cli:kafka")
	if err != nil {
		return err
	}
	if sub == "whoami" {
		cs, err := cl.Clusters(ctx)
		if err != nil {
			return err
		}
		user, _ := cl.User(ctx)
		if user == "" {
			user = "(SSO)"
		}
		fmt.Printf("Kafka UI %s: %s\n", cfg.Kafka.Env, user)
		for _, c := range cs {
			fmt.Printf("  %s  %s · %d brokers · %d topics · kafka %s\n", c.Name, c.Status, c.BrokerCount, c.TopicCount, c.Version)
		}
		return nil
	}
	cluster, err := kafkaCluster(ctx, cfg, cl)
	if err != nil {
		return err
	}
	need := func(what string) (string, error) {
		if len(pos) != 1 {
			return "", fmt.Errorf("usage: taskpilot kafka %s %s", sub, what)
		}
		return pos[0], nil
	}
	switch sub {
	case "topics":
		s := ""
		if len(pos) > 0 {
			s = pos[0]
		}
		ts, err := cl.Topics(ctx, cluster, s)
		if err != nil {
			return err
		}
		fmt.Print(kafbat.FormatTopics(ts))
		return nil
	case "topic":
		t, err := need("TOPIC")
		if err != nil {
			return err
		}
		td, err := cl.Topic(ctx, cluster, t)
		if err != nil {
			return err
		}
		fmt.Print(kafbat.FormatTopic(td))
		return nil
	case "messages":
		t, err := need("TOPIC")
		if err != nil {
			return err
		}
		q := kafbat.MessagesQuery{Limit: *limit, Filter: *filter}
		if *partition >= 0 {
			q.Partitions = []int{*partition}
		}
		var off *int64
		if *fromOffset >= 0 {
			off = fromOffset
		}
		if err := q.Start(off, *since, time.Now()); err != nil {
			return err
		}
		ms, err := cl.Messages(ctx, cluster, t, q)
		if err != nil {
			return err
		}
		if len(ms) == 0 {
			fmt.Println("(không có message)")
			return nil
		}
		fmt.Print(kafbat.FormatMessages(ms, 0))
		return nil
	case "groups":
		t, err := need("TOPIC")
		if err != nil {
			return err
		}
		gs, err := cl.TopicGroups(ctx, cluster, t)
		if err != nil {
			return err
		}
		fmt.Print(kafbat.FormatGroups(gs))
		return nil
	case "group":
		id, err := need("ID")
		if err != nil {
			return err
		}
		g, err := cl.Group(ctx, cluster, id)
		if err != nil {
			return err
		}
		fmt.Print(kafbat.FormatGroup(g, nil))
		return nil
	}
	fmt.Print(kafkaUsage)
	return fmt.Errorf("unknown kafka command %q", sub)
}
