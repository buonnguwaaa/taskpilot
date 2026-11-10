// Package qcdb gives agents read-only access to a project's QC database
// (MySQL). The credentials are read from a file the project's repos already
// keep (a KEY=VALUE env file or a YAML config) or from the taskpilot secret
// store; they never reach the agent. Every statement is checked to be a read
// and runs in a READ ONLY transaction with row, size and time limits.
package qcdb

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
	"gopkg.in/yaml.v3"

	"github.com/buonnguwaaa/taskpilot/internal/config"
)

// SecretName is where a project's DSN is kept when it is not in a repo file.
func SecretName(project string) string { return "qc-db/" + project }

// ErrNotConfigured means the project has no qc_db section.
var ErrNotConfigured = errors.New("this project has no QC database configured (qc_db in the taskpilot config)")

// DSN builds the connection string of a project's QC database. secret
// returns the DSN stored with `taskpilot auth set-db` (used when the config
// names no file).
func DSN(ctx context.Context, project string, c *config.QCDB, secret func(ctx context.Context, name string) (string, error)) (string, error) {
	if c == nil {
		return "", ErrNotConfigured
	}
	var raw string
	switch {
	case c.File == "":
		if secret == nil {
			return "", fmt.Errorf("no DSN stored for %s: run `taskpilot auth set-db %s`", project, project)
		}
		s, err := secret(ctx, SecretName(project))
		if err != nil {
			return "", fmt.Errorf("no DSN stored for %s (run `taskpilot auth set-db %s`): %w", project, project, err)
		}
		raw = s
	case c.Env != "":
		v, err := envValue(c.File, c.Env)
		if err != nil {
			return "", err
		}
		raw = v
	case c.YAML != nil:
		v, err := yamlDSN(c.File, c.YAML)
		if err != nil {
			return "", err
		}
		raw = v
	default:
		return "", fmt.Errorf("qc_db of %s: file %s needs env (the DSN key) or yaml (key paths)", project, c.File)
	}
	mc, err := mysql.ParseDSN(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("qc_db of %s: the DSN does not parse (check the file or the stored DSN)", project)
	}
	if c.Database != "" {
		mc.DBName = c.Database
	}
	mc.MultiStatements = false
	mc.AllowAllFiles = false
	mc.Timeout = 10 * time.Second
	mc.ReadTimeout = c.Timeout + 5*time.Second
	mc.ParseTime = false // show DATETIME as written
	mc.Loc = time.Local
	return mc.FormatDSN(), nil
}

// envValue reads KEY=VALUE from an env file (quotes and "export " allowed).
func envValue(path, key string) (string, error) {
	f, err := os.Open(config.ExpandHome(path))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(sc.Text()), "export "))
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != key || strings.HasPrefix(line, "#") {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		return v, nil
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("%s has no %s", path, key)
}

// yamlDSN builds a DSN from the dotted key paths of a YAML config.
func yamlDSN(path string, k *config.QCDBYAML) (string, error) {
	b, err := os.ReadFile(config.ExpandHome(path))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return "", fmt.Errorf("parse %s: %w", path, err)
	}
	get := func(p string) (string, error) {
		if p == "" {
			return "", nil
		}
		var cur any = doc
		for _, part := range strings.Split(p, ".") {
			m, ok := cur.(map[string]any)
			if !ok {
				return "", fmt.Errorf("%s has no %s", path, p)
			}
			if cur, ok = m[part]; !ok {
				return "", fmt.Errorf("%s has no %s", path, p)
			}
		}
		return fmt.Sprint(cur), nil
	}
	v := map[string]string{}
	for name, p := range map[string]string{"host": k.Host, "port": k.Port, "user": k.User, "password": k.Password, "database": k.Database} {
		s, err := get(p)
		if err != nil {
			return "", err
		}
		v[name] = s
	}
	if v["host"] == "" || v["user"] == "" {
		return "", fmt.Errorf("%s: host and user key paths are required", path)
	}
	if v["port"] == "" {
		v["port"] = "3306"
	}
	mc := mysql.NewConfig()
	mc.User, mc.Passwd, mc.Net, mc.Addr, mc.DBName = v["user"], v["password"], "tcp", net.JoinHostPort(v["host"], v["port"]), v["database"]
	return mc.FormatDSN(), nil
}

var (
	readFirst = regexp.MustCompile(`(?i)^(select|show|describe|desc|explain|with)\b`)
	// Statement keywords that write or change state. Function names that
	// share a word (REPLACE(…)) are blanked first; SET is left out because
	// CHARACTER SET appears in reads (and SET cannot follow a SELECT).
	writeWord   = regexp.MustCompile(`(?i)\b(insert|update|delete|replace|drop|alter|create|truncate|rename|grant|revoke|call|handler|prepare|execute|deallocate|kill|shutdown|load\s+data|lock\s+tables)\b`)
	funcCall    = regexp.MustCompile(`(?i)\b(replace|insert)\s*\(`)
	sideEffects = regexp.MustCompile(`(?i)\binto\s+(outfile|dumpfile|@)|\bfor\s+update\b|\bfor\s+share\b|\block\s+in\s+share\s+mode\b|\b(sleep|benchmark|get_lock|release_lock|release_all_locks|load_file)\s*\(`)
	stringsRe   = regexp.MustCompile(`'(?:[^'\\]|\\.|'')*'|"(?:[^"\\]|\\.|"")*"|` + "`[^`]*`")
	commentsRe  = regexp.MustCompile(`/\*(?s:.*?)\*/|(?:--\s|#)[^\n]*`)
)

// Validate accepts one read statement: SELECT, SHOW, DESCRIBE or EXPLAIN
// (WITH … SELECT included). It only rejects early and explains why; what
// prevents writes is the READ ONLY transaction Query runs it in.
func Validate(q string) error {
	if strings.Contains(q, "/*!") {
		return errors.New("MySQL executable comments (/*! … */) are not allowed")
	}
	bare := stringsRe.ReplaceAllString(q, "''") // keywords inside literals and quoted names do not count
	bare = strings.TrimSpace(commentsRe.ReplaceAllString(bare, " "))
	bare = strings.TrimSpace(strings.TrimSuffix(bare, ";"))
	switch {
	case bare == "":
		return errors.New("empty statement")
	case strings.Contains(bare, ";"):
		return errors.New("one statement at a time")
	case !readFirst.MatchString(bare):
		return errors.New("read-only: only SELECT, SHOW, DESCRIBE and EXPLAIN are allowed")
	}
	if m := writeWord.FindString(funcCall.ReplaceAllString(bare, "f(")); m != "" {
		return fmt.Errorf("read-only: %s is not allowed", strings.ToUpper(m))
	}
	if m := sideEffects.FindString(bare); m != "" {
		return fmt.Errorf("read-only: %q is not allowed", strings.TrimSpace(m))
	}
	return nil
}

// Result is a query result, already limited.
type Result struct {
	Columns   []string
	Rows      [][]string
	Truncated bool // more rows than the limit
	Elapsed   time.Duration
}

// Query runs one validated read statement in a READ ONLY transaction.
func Query(ctx context.Context, dsn, q string, maxRows int, timeout time.Duration) (*Result, error) {
	if err := Validate(q); err != nil {
		return nil, err
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer conn.Close()
	// The whole session is read-only, not only the transaction: fail closed
	// if the server refuses.
	if _, err := conn.ExecContext(ctx, "SET SESSION TRANSACTION READ ONLY"); err != nil {
		return nil, fmt.Errorf("could not make the session read-only, not running the query: %w", err)
	}
	// Server-side time limits for SELECTs: MySQL 5.7.8+ (ms), MariaDB 10.1+ (s).
	// Each is ignored by the other server.
	_, _ = conn.ExecContext(ctx, "SET SESSION max_execution_time = "+strconv.Itoa(int(timeout.Milliseconds())))
	_, _ = conn.ExecContext(ctx, "SET SESSION max_statement_time = "+strconv.FormatFloat(timeout.Seconds(), 'f', 0, 64))
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin read-only transaction: %w", err)
	}
	defer tx.Rollback()
	start := time.Now()
	rows, err := tx.QueryContext(ctx, strings.TrimSuffix(strings.TrimSpace(q), ";"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	res := &Result{Columns: cols}
	vals := make([]sql.RawBytes, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if len(res.Rows) == maxRows {
			res.Truncated = true
			break
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make([]string, len(cols))
		for i, v := range vals {
			row[i] = cell(v)
		}
		res.Rows = append(res.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	res.Elapsed = time.Since(start)
	return res, nil
}

const maxCell = 300

func cell(b sql.RawBytes) string {
	if b == nil {
		return "NULL"
	}
	if !utf8.Valid(b) {
		return fmt.Sprintf("<%d bytes binary>", len(b))
	}
	s := strings.NewReplacer("\t", " ", "\r", " ", "\n", "⏎").Replace(string(b))
	if r := []rune(s); len(r) > maxCell {
		s = string(r[:maxCell]) + "…"
	}
	return s
}

// Format renders a result as tab-separated lines with a header.
func Format(r *Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d row(s) in %s", len(r.Rows), r.Elapsed.Round(time.Millisecond))
	if r.Truncated {
		b.WriteString(" — limit reached, more rows exist: narrow the WHERE or aggregate")
	}
	b.WriteString("\n")
	if len(r.Columns) > 0 {
		b.WriteString(strings.Join(r.Columns, "\t") + "\n")
	}
	for _, row := range r.Rows {
		b.WriteString(strings.Join(row, "\t") + "\n")
	}
	return b.String()
}

// Target describes where a DSN points, without credentials (for logs and UI).
func Target(dsn string) string {
	mc, err := mysql.ParseDSN(dsn)
	if err != nil {
		return "?"
	}
	return mc.Addr + "/" + mc.DBName
}
