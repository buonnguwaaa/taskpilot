package main

import (
	"archive/tar"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/buonnguwaaa/taskpilot/internal/config"
)

// manifest describes an export archive.
type manifest struct {
	ExportedAt      time.Time `json:"exported_at"`
	Home            string    `json:"home"`      // taskpilot home on the source machine
	UserHome        string    `json:"user_home"` // $HOME on the source machine
	SchemaVersion   int       `json:"schema_version"`
	SecretsIncluded bool      `json:"secrets_included"`
	Sessions        []string  `json:"claude_sessions"`
}

// cmdExport packs what another machine needs to continue: a consistent
// snapshot of the database, the task blobs (events, logs, file-based tasks
// not migrated yet), the Claude Code transcripts of task sessions (needed by
// --resume) and the config. Secrets go to a separate archive, only with -secrets.
func cmdExport(args []string) error {
	c := newFlags("export")
	out := c.fs.String("o", "", "output directory (default ~/taskpilot-export)")
	withSecrets := c.fs.Bool("secrets", false, "also write a separate archive with the tokens (plain text: never commit it)")
	c.fs.Parse(args)
	cfg, err := config.Load(*c.config)
	if err != nil {
		return err
	}
	dir := *out
	if dir == "" {
		dir = config.ExpandHome("~/taskpilot-export")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	stamp := time.Now().Format("20060102-150405")
	tmp, err := os.MkdirTemp(dir, ".export-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	// 1. Consistent snapshot, read-only on the live database (no migration, no locks held long).
	snap := filepath.Join(tmp, "taskpilot.db")
	if err := snapshotDB(filepath.Join(cfg.Home, "taskpilot.db"), snap); err != nil {
		return fmt.Errorf("snapshot database: %w", err)
	}
	var secretsSnap string
	if *withSecrets {
		secretsSnap = filepath.Join(tmp, "secrets.db")
		if err := copyFile(snap, secretsSnap, 0o600); err != nil {
			return err
		}
		if err := keepOnlySecrets(secretsSnap); err != nil {
			return err
		}
	}
	if err := stripSecrets(snap); err != nil {
		return err
	}
	version, _ := schemaVersion(snap)

	// 2. Claude Code transcripts of the tasks' sessions.
	sessions, err := taskSessions(cfg.Home)
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	var transcripts []string
	for _, sid := range sessions {
		matches, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", sid+".jsonl"))
		transcripts = append(transcripts, matches...)
	}

	m := manifest{ExportedAt: time.Now().UTC(), Home: cfg.Home, UserHome: home, SchemaVersion: version, Sessions: sessions}
	dataPath := filepath.Join(dir, "taskpilot-data-"+stamp+".tar.gz")
	err = writeArchive(dataPath, func(tw *tar.Writer) error {
		mb, _ := json.MarshalIndent(m, "", "  ")
		if err := addBytes(tw, "manifest.json", mb); err != nil {
			return err
		}
		if err := addFile(tw, snap, "taskpilot.db"); err != nil {
			return err
		}
		if err := addTree(tw, filepath.Join(cfg.Home, "tasks"), "tasks"); err != nil {
			return err
		}
		for _, t := range transcripts {
			rel, _ := filepath.Rel(filepath.Join(home, ".claude", "projects"), t)
			if err := addFile(tw, t, filepath.Join("claude-projects", rel)); err != nil {
				return err
			}
		}
		return addFile(tw, cfg.Path, "config/config.yaml")
	})
	if err != nil {
		return err
	}
	fmt.Printf("dữ liệu (KHÔNG có token): %s\n", dataPath)
	fmt.Printf("  database schema v%d, thư mục tasks/, %d transcript Claude, config\n", version, len(transcripts))
	if secretsSnap != "" {
		m.SecretsIncluded = true
		secPath := filepath.Join(dir, "taskpilot-secrets-"+stamp+".tar.gz")
		err = writeArchive(secPath, func(tw *tar.Writer) error {
			mb, _ := json.MarshalIndent(m, "", "  ")
			if err := addBytes(tw, "manifest.json", mb); err != nil {
				return err
			}
			return addFile(tw, secretsSnap, "secrets.db")
		})
		if err != nil {
			return err
		}
		fmt.Printf("token (văn bản thường — KHÔNG commit, xoá sau khi import): %s\n", secPath)
	}
	fmt.Println("worktree không được đóng gói: commit của agent nằm trong nhánh tp/<task> của các repo — push các nhánh đó nếu task đang làm dở.")
	return nil
}

func openRaw(path string, readOnly bool) (*sql.DB, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)"
	if readOnly {
		dsn += "&mode=ro"
	}
	return sql.Open("sqlite", dsn)
}

// snapshotDB writes a transactionally consistent copy with VACUUM INTO.
func snapshotDB(src, dst string) error {
	db, err := openRaw(src, true)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec(`VACUUM INTO ?`, dst); err != nil {
		return err
	}
	return os.Chmod(dst, 0o600)
}

// stripSecrets removes token values and rewrites the file so no deleted value
// survives in free pages.
func stripSecrets(path string) error {
	db, err := openRaw(path, false)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec(`DELETE FROM secrets`); err != nil {
		return err
	}
	_, err = db.Exec(`VACUUM`)
	return err
}

// keepOnlySecrets reduces a snapshot to the secrets tables.
func keepOnlySecrets(path string) error {
	db, err := openRaw(path, false)
	if err != nil {
		return err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT type, name FROM sqlite_master WHERE type IN ('table','view') AND name NOT LIKE 'sqlite_%'
		AND name NOT IN ('secrets','secret_access_log','encryption_keys','secret_bindings')`)
	if err != nil {
		return err
	}
	var drops []string
	for rows.Next() {
		var typ, name string
		rows.Scan(&typ, &name)
		drops = append(drops, fmt.Sprintf(`DROP %s IF EXISTS "%s"`, strings.ToUpper(typ), name))
	}
	rows.Close()
	db.Exec(`PRAGMA foreign_keys = OFF`)
	for _, d := range drops {
		if strings.HasPrefix(d, "DROP VIEW") {
			if _, err := db.Exec(d); err != nil {
				return err
			}
		}
	}
	for _, d := range drops {
		if strings.HasPrefix(d, "DROP TABLE") {
			if _, err := db.Exec(d); err != nil {
				return err
			}
		}
	}
	_, err = db.Exec(`VACUUM`)
	return err
}

func schemaVersion(path string) (int, error) {
	db, err := openRaw(path, true)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var v int
	err = db.QueryRow(`PRAGMA user_version`).Scan(&v)
	return v, err
}

// taskSessions collects the Claude session ids of all tasks, from the database
// (schema v2) and from file-based tasks not migrated yet.
func taskSessions(home string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	if db, err := openRaw(filepath.Join(home, "taskpilot.db"), true); err == nil {
		if rows, err := db.Query(`SELECT session_id FROM tasks`); err == nil {
			for rows.Next() {
				var s string
				rows.Scan(&s)
				add(s)
			}
			rows.Close()
		}
		db.Close()
	}
	files, _ := filepath.Glob(filepath.Join(home, "tasks", "*", "task.json"))
	legacy, _ := filepath.Glob(filepath.Join(home, "tasks", "*", "legacy", "task.json"))
	for _, f := range append(files, legacy...) {
		var t struct {
			SessionID string `json:"session_id"`
		}
		if b, err := os.ReadFile(f); err == nil && json.Unmarshal(b, &t) == nil {
			add(t.SessionID)
		}
	}
	return out, nil
}

func writeArchive(path string, fill func(*tar.Writer) error) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	if err := fill(tw); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return f.Close()
}

func addBytes(tw *tar.Writer, name string, b []byte) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(b)), ModTime: time.Now()}); err != nil {
		return err
	}
	_, err := tw.Write(b)
	return err
}

func addFile(tw *tar.Writer, src, name string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(name), Mode: 0o600, Size: st.Size(), ModTime: st.ModTime()}); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}

func addTree(tw *tar.Writer, root, prefix string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		return addFile(tw, p, filepath.Join(prefix, rel))
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// ---- import ------------------------------------------------------------------------

// cmdImport restores an export on this machine: the database (merging tokens
// from a secrets archive if given), the task blobs, the Claude transcripts and,
// if none exists yet, the config.
func cmdImport(args []string) error {
	c := newFlags("import")
	data := c.fs.String("i", "", "taskpilot-data-*.tar.gz")
	secrets := c.fs.String("secrets", "", "optional taskpilot-secrets-*.tar.gz")
	force := c.fs.Bool("force", false, "replace an existing taskpilot home")
	c.fs.Parse(args)
	if *data == "" {
		return errors.New("usage: taskpilot import -i taskpilot-data-….tar.gz [-secrets taskpilot-secrets-….tar.gz]")
	}
	stage, err := os.MkdirTemp("", "taskpilot-import-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := extract(*data, stage); err != nil {
		return err
	}
	var m manifest
	if b, err := os.ReadFile(filepath.Join(stage, "manifest.json")); err != nil || json.Unmarshal(b, &m) != nil {
		return errors.New("not a taskpilot export (manifest.json missing)")
	}
	cfgPath := config.ExpandHome(*c.config)
	if _, err := os.Stat(cfgPath); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
			return err
		}
		if err := copyFile(filepath.Join(stage, "config", "config.yaml"), cfgPath, 0o644); err != nil {
			return err
		}
		fmt.Printf("config: %s (từ máy cũ — kiểm tra đường dẫn repo và chrome_profile)\n", cfgPath)
	} else {
		alt := cfgPath + ".imported"
		copyFile(filepath.Join(stage, "config", "config.yaml"), alt, 0o644)
		fmt.Printf("config hiện có được giữ nguyên; bản từ máy cũ: %s\n", alt)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	dbPath := filepath.Join(cfg.Home, "taskpilot.db")
	if _, err := os.Stat(dbPath); err == nil && !*force {
		return fmt.Errorf("%s already exists; use -force to replace it (stop taskpilot first)", dbPath)
	}
	if err := os.MkdirAll(cfg.Home, 0o700); err != nil {
		return err
	}
	if *force {
		os.Remove(dbPath + "-wal")
		os.Remove(dbPath + "-shm")
	}
	if err := copyFile(filepath.Join(stage, "taskpilot.db"), dbPath, 0o600); err != nil {
		return err
	}
	if err := copyTree(filepath.Join(stage, "tasks"), filepath.Join(cfg.Home, "tasks")); err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	if err := copyTree(filepath.Join(stage, "claude-projects"), filepath.Join(home, ".claude", "projects")); err != nil {
		return err
	}
	if *secrets != "" {
		sstage := filepath.Join(stage, "secrets")
		if err := extract(*secrets, sstage); err != nil {
			return err
		}
		db, err := openRaw(dbPath, false)
		if err != nil {
			return err
		}
		_, err = db.Exec(`ATTACH DATABASE ? AS s`, filepath.Join(sstage, "secrets.db"))
		if err == nil {
			_, err = db.Exec(`INSERT OR REPLACE INTO secrets SELECT * FROM s.secrets`)
		}
		db.Close()
		if err != nil {
			return fmt.Errorf("import secrets: %w", err)
		}
		fmt.Println("token đã được nạp. Hãy xoá file taskpilot-secrets-….tar.gz.")
	}
	fmt.Printf("đã import vào %s (database, tasks/, %d transcript Claude)\n", cfg.Home, len(m.Sessions))
	if m.Home != cfg.Home || m.UserHome != home {
		fmt.Printf("lưu ý: máy cũ dùng home %s; workspace của task cũ trỏ tới đường dẫn đó.\n", m.Home)
	}
	fmt.Println("chạy `taskpilot` để tiếp tục (task dạng file sẽ được chuyển vào database lần đầu chạy).")
	return nil
}

func extract(archive, dst string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(h.Name)
		if filepath.IsAbs(name) || strings.HasPrefix(name, "..") {
			return fmt.Errorf("unsafe path in archive: %s", h.Name)
		}
		p := filepath.Join(dst, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		out, err := os.OpenFile(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, io.LimitReader(tr, 4<<30)); err != nil {
			out.Close()
			return err
		}
		out.Close()
	}
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if _, err := os.Stat(target); err == nil {
			return nil // keep what is already there
		}
		return copyFile(p, target, 0o644)
	})
}
