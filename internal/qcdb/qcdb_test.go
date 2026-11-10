package qcdb

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"

	"github.com/buonnguwaaa/taskpilot/internal/config"
)

func TestValidate(t *testing.T) {
	ok := []string{
		"SELECT * FROM transactions WHERE code = 'TX1' LIMIT 10",
		"select id from t;",
		"  -- comment\nSHOW TABLES",
		"DESCRIBE transactions",
		"desc t",
		"EXPLAIN SELECT 1",
		"WITH x AS (SELECT 1) SELECT * FROM x",
		"SELECT REPLACE(name, 'a', 'b'), CONVERT(n USING utf8mb4) FROM t",
		"SELECT * FROM t WHERE note = 'please delete; then update'", // keywords in a literal
		"SELECT `update`, updated_at, created_by FROM t",
		"SHOW CHARACTER SET",
	}
	for _, q := range ok {
		if err := Validate(q); err != nil {
			t.Errorf("%q rejected: %v", q, err)
		}
	}
	bad := []string{
		"", ";",
		"UPDATE t SET a = 1",
		"DELETE FROM t",
		"INSERT INTO t VALUES (1)",
		"DROP TABLE t",
		"SET @a = 1",
		"SELECT 1; DELETE FROM t",
		"WITH x AS (SELECT 1) DELETE FROM t",
		"SELECT * FROM t INTO OUTFILE '/tmp/x'",
		"SELECT * FROM t FOR UPDATE",
		"SELECT SLEEP(100)",
		"SELECT /*! 1; DROP TABLE t */ 1",
		"CALL p()",
		"LOAD DATA INFILE 'x' INTO TABLE t",
		"TRUNCATE t",
	}
	for _, q := range bad {
		if err := Validate(q); err == nil {
			t.Errorf("%q accepted", q)
		}
	}
}

func TestDSNSources(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".base.env")
	os.WriteFile(env, []byte("# c\nDB_DRIVER_NAME=mysql\nexport DB_DATA_SOURCE=\"u:p@ss@tcp(10.0.0.1:3306)/oms?charset=utf8&parseTime=True\"\n"), 0o600)
	yml := filepath.Join(dir, "local.yml")
	os.WriteFile(yml, []byte("MySql:\n  MySqlHost: db.qc\n  MySqlPort: 3307\n  MySqlUser: dev\n  MySqlPassword: s3cr@t\n  MySqlDbname: bom\n"), 0o600)
	ctx := context.Background()

	dsn, err := DSN(ctx, "payment", &config.QCDB{File: env, Env: "DB_DATA_SOURCE", Timeout: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	mc, _ := mysql.ParseDSN(dsn)
	if mc.User != "u" || mc.Passwd != "p@ss" || mc.Addr != "10.0.0.1:3306" || mc.DBName != "oms" || mc.MultiStatements || mc.ParseTime {
		t.Fatalf("env DSN = %+v", mc)
	}
	if Target(dsn) != "10.0.0.1:3306/oms" || strings.Contains(Target(dsn), "p@ss") {
		t.Fatalf("target = %q", Target(dsn))
	}

	dsn, err = DSN(ctx, "bom", &config.QCDB{File: yml, YAML: &config.QCDBYAML{Host: "MySql.MySqlHost", Port: "MySql.MySqlPort",
		User: "MySql.MySqlUser", Password: "MySql.MySqlPassword", Database: "MySql.MySqlDbname"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if mc, _ = mysql.ParseDSN(dsn); mc.Addr != "db.qc:3307" || mc.Passwd != "s3cr@t" || mc.DBName != "bom" {
		t.Fatalf("yaml DSN = %+v", mc)
	}

	secret := func(_ context.Context, name string) (string, error) {
		if name != "qc-db/plm" {
			return "", errors.New("no secret")
		}
		return "plm:x@tcp(plm.qc:3306)/plm", nil
	}
	if dsn, err = DSN(ctx, "plm", &config.QCDB{Database: "plm2"}, secret); err != nil || Target(dsn) != "plm.qc:3306/plm2" {
		t.Fatalf("secret DSN = %q %v", Target(dsn), err)
	}
	if _, err = DSN(ctx, "bom", &config.QCDB{}, secret); err == nil || !strings.Contains(err.Error(), "auth set-db bom") {
		t.Fatalf("missing secret: %v", err)
	}
	if _, err = DSN(ctx, "x", nil, nil); !errors.Is(err, ErrNotConfigured) {
		t.Fatal("no qc_db: ErrNotConfigured")
	}
	if _, err = DSN(ctx, "payment", &config.QCDB{File: env, Env: "NOPE"}, nil); err == nil {
		t.Fatal("missing key must fail")
	}
}

func TestFormatAndCell(t *testing.T) {
	r := &Result{Columns: []string{"id", "note"}, Rows: [][]string{{"1", cell([]byte("a\tb\nc"))}, {"2", cell(nil)}}, Truncated: true}
	out := Format(r)
	if !strings.Contains(out, "id\tnote") || !strings.Contains(out, "1\ta b⏎c") || !strings.Contains(out, "2\tNULL") || !strings.Contains(out, "limit reached") {
		t.Fatalf("format:\n%s", out)
	}
	if got := cell([]byte{0xff, 0xfe}); !strings.Contains(got, "binary") {
		t.Fatalf("binary cell = %q", got)
	}
}
