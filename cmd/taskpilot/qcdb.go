package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/go-sql-driver/mysql"

	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/db"
	"github.com/buonnguwaaa/taskpilot/internal/qcdb"
)

// secretReader returns a function reading one secret from the store.
func secretReader(cfg *config.Config, consumer string) func(ctx context.Context, name string) (string, error) {
	return func(ctx context.Context, name string) (string, error) {
		v, _, err := storedSecret(ctx, cfg, name, consumer, nil)
		return v, err
	}
}

// storedSecret reads a secret from the operator's store; a missing one is
// the integration's own "not logged in" error (missing), when given.
func storedSecret(ctx context.Context, cfg *config.Config, name, consumer string, missing error) (string, *db.SecretMeta, error) {
	d, err := db.Open(ctx, cfg.Home)
	if err != nil {
		return "", nil, err
	}
	defer d.Close()
	v, meta, err := d.Resolve(ctx, name, consumer)
	if missing != nil && errors.Is(err, db.ErrNoSecret) {
		return "", nil, missing
	}
	return v, meta, err
}

// authSetDB stores the DSN of a project's QC database (for a project whose
// repos keep no credentials file).
func authSetDB(ctx context.Context, cfg *config.Config, d *db.DB, project string) error {
	p, ok := cfg.Projects[project]
	if !ok {
		return fmt.Errorf("unknown project %q (configured: %s)", project, strings.Join(cfg.ProjectNames(), ", "))
	}
	v, err := readSecret("Dán DSN MySQL của DB QC " + p.Name + " (user:pass@tcp(host:3306)/db) rồi Enter: ")
	if err != nil {
		return err
	}
	mc, err := mysql.ParseDSN(strings.TrimSpace(v))
	if err != nil {
		return fmt.Errorf("DSN không hợp lệ (dạng user:pass@tcp(host:3306)/db)")
	}
	if err := d.SetPlain(ctx, qcdb.SecretName(project), strings.TrimSpace(v), "QC database DSN of "+p.Name, nil, "operator"); err != nil {
		return err
	}
	fmt.Printf("đã lưu DSN DB QC của %s (%s/%s)\n", p.Name, mc.Addr, mc.DBName)
	if p.QCDB == nil {
		fmt.Printf("thêm vào config để agent dùng được:\n  projects:\n    %s:\n      qc_db: {}\n", project)
	} else if p.QCDB.File != "" {
		fmt.Printf("lưu ý: config của %s đang đọc DSN từ %s; bỏ qc_db.file để dùng DSN vừa lưu\n", project, p.QCDB.File)
	}
	return nil
}

const qcdbUsage = `taskpilot qcdb <command>

  list                   the QC database of each project (target only, never credentials)
  query PROJECT "SQL"    run one read-only statement (SELECT, SHOW, DESCRIBE, EXPLAIN)
`

func cmdQCDB(args []string) error {
	if len(args) == 0 {
		fmt.Print(qcdbUsage)
		return nil
	}
	sub, args := args[0], args[1:]
	c := newFlags("qcdb " + sub)
	pos := c.parse(args)
	cfg, err := config.Load(*c.config)
	if err != nil {
		return err
	}
	ctx := context.Background()
	secret := secretReader(cfg, "cli:qcdb")
	switch sub {
	case "list":
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "PROJECT\tSOURCE\tTARGET")
		for _, name := range cfg.ProjectNames() {
			q := cfg.Projects[name].QCDB
			if q == nil {
				fmt.Fprintf(w, "%s\t-\t(không cấu hình)\n", name)
				continue
			}
			src := "secret " + qcdb.SecretName(name)
			if q.File != "" {
				src = q.File
			}
			target := "?"
			if dsn, err := qcdb.DSN(ctx, name, q, secret); err != nil {
				target = "lỗi: " + err.Error()
			} else {
				target = qcdb.Target(dsn)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", name, src, target)
		}
		return w.Flush()
	case "query":
		if len(pos) != 2 {
			return fmt.Errorf(`usage: taskpilot qcdb query PROJECT "SELECT …"`)
		}
		p, ok := cfg.Projects[pos[0]]
		if !ok {
			return fmt.Errorf("unknown project %q", pos[0])
		}
		dsn, err := qcdb.DSN(ctx, pos[0], p.QCDB, secret)
		if err != nil {
			return err
		}
		max := 200
		if p.QCDB.MaxRows > 0 {
			max = p.QCDB.MaxRows
		}
		res, err := qcdb.Query(ctx, dsn, pos[1], max, p.QCDB.Timeout)
		if err != nil {
			return err
		}
		fmt.Printf("%s (read-only)\n%s", qcdb.Target(dsn), qcdb.Format(res))
		return nil
	}
	fmt.Print(qcdbUsage)
	return fmt.Errorf("unknown qcdb command %q", sub)
}
