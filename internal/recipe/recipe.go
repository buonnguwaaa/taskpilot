// Package recipe keeps the debugging procedures the operator repeats ("check
// order X": its rows in the QC database, its production logs, the QC pod
// logs) as reusable recipes. A recipe is a list of read-only lookups with
// parameters, that taskpilot runs itself (the operator from the Logs tab, an
// agent with the MCP tool run_recipe), plus a guide on reading the results,
// shipped to agents as a skill. Reflect (reflect.go) proposes recipes from
// the history of tasks; nothing is used until the operator accepts it.
//
// Every step goes through the checks of the matching agent tool (read-only
// SQL, Splunk validation and the project's prod filter, vault_paths,
// kafka_topics, the project's QC apps), so a recipe can do nothing an agent
// of the project could not.
package recipe

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/buonnguwaaa/taskpilot/internal/qcdb"
	"github.com/buonnguwaaa/taskpilot/internal/splunk"
)

// Recipe is one reusable procedure of a project.
type Recipe struct {
	Name        string  `yaml:"name"`
	Project     string  `yaml:"project"`
	Description string  `yaml:"description"` // what it answers; agents read it to decide when to use it
	Params      []Param `yaml:"params,omitempty"`
	Steps       []Step  `yaml:"steps"`
	Guide       string  `yaml:"guide,omitempty"` // how to read the results, what to do next
}

// Param is a value the operator (or agent) gives when running the recipe;
// steps use it as {name}.
type Param struct {
	Name    string `yaml:"name"`
	Desc    string `yaml:"desc,omitempty"`
	Example string `yaml:"example,omitempty"`
}

// Step is one lookup: exactly one of the kinds is set.
type Step struct {
	Title  string      `yaml:"title,omitempty"`
	SQL    *SQLStep    `yaml:"sql,omitempty"`
	Splunk *SplunkStep `yaml:"splunk,omitempty"`
	QCLogs *QCLogsStep `yaml:"qc_logs,omitempty"`
	Vault  *VaultStep  `yaml:"vault,omitempty"`
	Kafka  *KafkaStep  `yaml:"kafka,omitempty"`
}

// SQLStep is a read-only statement on the project's QC database.
type SQLStep struct {
	Query   string `yaml:"query"`
	MaxRows int    `yaml:"max_rows,omitempty"`
}

// SplunkStep is a production log search (the project's filter is added).
type SplunkStep struct {
	Query     string `yaml:"query"`
	Earliest  string `yaml:"earliest,omitempty"`
	Latest    string `yaml:"latest,omitempty"`
	MaxEvents int    `yaml:"max_events,omitempty"`
}

// QCLogsStep reads recent pod logs of a QC application (Argo CD).
type QCLogsStep struct {
	App          string `yaml:"app"`
	Filter       string `yaml:"filter,omitempty"`
	Tail         int    `yaml:"tail,omitempty"`
	SinceMinutes int    `yaml:"since_minutes,omitempty"`
}

// VaultStep reads one QC Vault secret (keys and values).
type VaultStep struct {
	Path string `yaml:"path"`
}

// KafkaStep reads messages of a QC Kafka topic.
type KafkaStep struct {
	Topic  string `yaml:"topic"`
	Filter string `yaml:"filter,omitempty"`
	Since  string `yaml:"since,omitempty"`
	Limit  int    `yaml:"limit,omitempty"`
}

// Kind names the step's lookup.
func (s Step) Kind() string {
	switch {
	case s.SQL != nil:
		return "sql"
	case s.Splunk != nil:
		return "splunk"
	case s.QCLogs != nil:
		return "qc_logs"
	case s.Vault != nil:
		return "vault"
	case s.Kafka != nil:
		return "kafka"
	}
	return ""
}

func (s Step) kinds() int {
	n := 0
	for _, set := range []bool{s.SQL != nil, s.Splunk != nil, s.QCLogs != nil, s.Vault != nil, s.Kafka != nil} {
		if set {
			n++
		}
	}
	return n
}

// Command is the step in one line, as shown to the operator.
func (s Step) Command() string {
	switch {
	case s.SQL != nil:
		return "SQL QC: " + oneLine(s.SQL.Query)
	case s.Splunk != nil:
		w := s.Splunk.Earliest
		if w == "" {
			w = "mặc định"
		}
		return fmt.Sprintf("Splunk (%s): %s", w, oneLine(s.Splunk.Query))
	case s.QCLogs != nil:
		c := "log QC " + s.QCLogs.App
		if s.QCLogs.Filter != "" {
			c += fmt.Sprintf(" chứa %q", s.QCLogs.Filter)
		}
		return c
	case s.Vault != nil:
		return "Vault QC: " + s.Vault.Path
	case s.Kafka != nil:
		c := "Kafka QC " + s.Kafka.Topic
		if s.Kafka.Filter != "" {
			c += fmt.Sprintf(" chứa %q", s.Kafka.Filter)
		}
		return c
	}
	return "?"
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

var (
	nameRe        = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,59}$`)
	paramRe       = regexp.MustCompile(`^[a-z][a-z0-9_]{0,30}$`)
	placeholderRe = regexp.MustCompile(`\{([a-z][a-z0-9_]{0,30})\}`)
	// A value may hold codes, ids, emails, dates, words: never quotes,
	// backslashes, semicolons, pipes, brackets or newlines, so it cannot leave
	// the string it is put in (SQL, SPL).
	valueRe = regexp.MustCompile(`^[\p{L}\p{N} _.,:@/+=#%-]{1,200}$`)
)

// fields returns pointers to every text field of the step, for placeholders.
func (s *Step) fields() []*string {
	switch {
	case s.SQL != nil:
		return []*string{&s.SQL.Query}
	case s.Splunk != nil:
		return []*string{&s.Splunk.Query, &s.Splunk.Earliest, &s.Splunk.Latest}
	case s.QCLogs != nil:
		return []*string{&s.QCLogs.App, &s.QCLogs.Filter}
	case s.Vault != nil:
		return []*string{&s.Vault.Path}
	case s.Kafka != nil:
		return []*string{&s.Kafka.Topic, &s.Kafka.Filter, &s.Kafka.Since}
	}
	return nil
}

// clone copies the step deeply (Expand edits the copy).
func (s Step) clone() Step {
	c := s
	switch {
	case s.SQL != nil:
		v := *s.SQL
		c.SQL = &v
	case s.Splunk != nil:
		v := *s.Splunk
		c.Splunk = &v
	case s.QCLogs != nil:
		v := *s.QCLogs
		c.QCLogs = &v
	case s.Vault != nil:
		v := *s.Vault
		c.Vault = &v
	case s.Kafka != nil:
		v := *s.Kafka
		c.Kafka = &v
	}
	return c
}

// Validate checks a recipe: a known project (known may be nil), named
// params all used, one lookup per step, read-only SQL and valid SPL once the
// params are filled with their examples.
func Validate(r *Recipe, known func(project string) bool) error {
	if !nameRe.MatchString(r.Name) {
		return fmt.Errorf("tên %q: chỉ chữ thường, số và '-', 2-60 ký tự", r.Name)
	}
	if strings.TrimSpace(r.Project) == "" {
		return errors.New("thiếu project")
	}
	if known != nil && !known(r.Project) {
		return fmt.Errorf("project %q không có trong config", r.Project)
	}
	if strings.TrimSpace(r.Description) == "" {
		return errors.New("thiếu description (agent đọc nó để biết khi nào dùng)")
	}
	if len(r.Steps) == 0 {
		return errors.New("cần ít nhất một bước")
	}
	declared := map[string]bool{}
	sample := map[string]string{}
	for _, p := range r.Params {
		if !paramRe.MatchString(p.Name) {
			return fmt.Errorf("tham số %q: chữ thường, số, '_'", p.Name)
		}
		if declared[p.Name] {
			return fmt.Errorf("tham số %q bị trùng", p.Name)
		}
		declared[p.Name] = true
		sample[p.Name] = "x"
		if p.Example != "" && valueRe.MatchString(p.Example) {
			sample[p.Name] = p.Example
		}
	}
	used := map[string]bool{}
	for i, s := range r.Steps {
		n := i + 1
		if s.kinds() != 1 {
			return fmt.Errorf("bước %d: cần đúng một loại (sql, splunk, qc_logs, vault, kafka)", n)
		}
		for _, f := range s.fields() {
			for _, m := range placeholderRe.FindAllStringSubmatch(*f, -1) {
				if !declared[m[1]] {
					return fmt.Errorf("bước %d dùng {%s} nhưng không khai báo trong params", n, m[1])
				}
				used[m[1]] = true
			}
		}
		e := s.clone()
		for _, f := range e.fields() {
			*f = fill(*f, sample)
		}
		if err := checkStep(e); err != nil {
			return fmt.Errorf("bước %d: %w", n, err)
		}
	}
	for _, p := range r.Params {
		if !used[p.Name] {
			return fmt.Errorf("tham số %q không được bước nào dùng", p.Name)
		}
	}
	return nil
}

// checkStep checks one filled step's required fields and query.
func checkStep(s Step) error {
	switch {
	case s.SQL != nil:
		if strings.TrimSpace(s.SQL.Query) == "" {
			return errors.New("sql.query trống")
		}
		return qcdb.Validate(s.SQL.Query)
	case s.Splunk != nil:
		if strings.TrimSpace(s.Splunk.Query) == "" {
			return errors.New("splunk.query trống")
		}
		return splunk.Validate(s.Splunk.Query)
	case s.QCLogs != nil:
		if strings.TrimSpace(s.QCLogs.App) == "" {
			return errors.New("qc_logs.app trống")
		}
	case s.Vault != nil:
		if strings.TrimSpace(s.Vault.Path) == "" {
			return errors.New("vault.path trống")
		}
	case s.Kafka != nil:
		if strings.TrimSpace(s.Kafka.Topic) == "" {
			return errors.New("kafka.topic trống")
		}
	}
	return nil
}

func fill(s string, vals map[string]string) string {
	return placeholderRe.ReplaceAllStringFunc(s, func(m string) string {
		if v, ok := vals[m[1:len(m)-1]]; ok {
			return v
		}
		return m
	})
}

// Expand fills the steps with the given values (every param is required and
// checked), and checks the filled steps.
func (r *Recipe) Expand(vals map[string]string) ([]Step, error) {
	clean := map[string]string{}
	for _, p := range r.Params {
		v := strings.TrimSpace(vals[p.Name])
		if v == "" {
			return nil, fmt.Errorf("thiếu giá trị cho %s", p.Name)
		}
		if !valueRe.MatchString(v) {
			return nil, fmt.Errorf("%s: giá trị không được chứa nháy, \\, ;, |, ngoặc hay xuống dòng", p.Name)
		}
		clean[p.Name] = v
	}
	out := make([]Step, len(r.Steps))
	for i, s := range r.Steps {
		e := s.clone()
		for _, f := range e.fields() {
			*f = fill(*f, clean)
		}
		if err := checkStep(e); err != nil {
			return nil, fmt.Errorf("bước %d: %w", i+1, err)
		}
		out[i] = e
	}
	return out, nil
}

// Exec runs the lookups, with the checks and audit of the agent tools.
type Exec interface {
	SQL(ctx context.Context, s SQLStep) (string, error)
	Splunk(ctx context.Context, s SplunkStep) (string, error)
	QCLogs(ctx context.Context, s QCLogsStep) (string, error)
	Vault(ctx context.Context, s VaultStep) (string, error)
	Kafka(ctx context.Context, s KafkaStep) (string, error)
}

// StepResult is the outcome of one step.
type StepResult struct {
	Title   string
	Command string
	Output  string
	Err     error
}

// stepOut bounds one step's output.
const stepOut = 12000

// Run expands and runs every step in order; a failing step does not stop the
// next ones.
func Run(ctx context.Context, r *Recipe, vals map[string]string, x Exec) ([]StepResult, error) {
	steps, err := r.Expand(vals)
	if err != nil {
		return nil, err
	}
	var out []StepResult
	for i, s := range steps {
		res := StepResult{Title: s.Title, Command: s.Command()}
		if res.Title == "" {
			res.Title = fmt.Sprintf("Bước %d", i+1)
		}
		switch {
		case s.SQL != nil:
			res.Output, res.Err = x.SQL(ctx, *s.SQL)
		case s.Splunk != nil:
			res.Output, res.Err = x.Splunk(ctx, *s.Splunk)
		case s.QCLogs != nil:
			res.Output, res.Err = x.QCLogs(ctx, *s.QCLogs)
		case s.Vault != nil:
			res.Output, res.Err = x.Vault(ctx, *s.Vault)
		case s.Kafka != nil:
			res.Output, res.Err = x.Kafka(ctx, *s.Kafka)
		}
		if len(res.Output) > stepOut {
			res.Output = res.Output[:stepOut] + "\n… (cắt bớt)"
		}
		out = append(out, res)
		if ctx.Err() != nil {
			break
		}
	}
	return out, nil
}

// Format renders the results as plain text (the operator's pane, the agent's tool result).
func Format(r *Recipe, vals map[string]string, res []StepResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Quy trình %s (%s)", r.Name, r.Project)
	var ps []string
	for _, p := range r.Params {
		ps = append(ps, p.Name+"="+vals[p.Name])
	}
	if len(ps) > 0 {
		b.WriteString(": " + strings.Join(ps, " "))
	}
	b.WriteString("\n")
	for i, s := range res {
		fmt.Fprintf(&b, "\n## %d. %s\n$ %s\n", i+1, s.Title, s.Command)
		if s.Err != nil {
			fmt.Fprintf(&b, "LỖI: %v\n", s.Err)
			continue
		}
		b.WriteString(strings.TrimRight(s.Output, "\n") + "\n")
	}
	if g := strings.TrimSpace(r.Guide); g != "" {
		b.WriteString("\n## Cách đọc kết quả\n" + g + "\n")
	}
	return b.String()
}

// ---- files ------------------------------------------------------------------

// Dir is where the accepted recipes live: one YAML file each.
func Dir(home string) string { return filepath.Join(home, "recipes") }

// Parse reads a recipe from YAML.
func Parse(b []byte) (*Recipe, error) {
	var r Recipe
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("đọc YAML: %w", err)
	}
	return &r, nil
}

// Marshal writes a recipe as YAML.
func Marshal(r *Recipe) string {
	b, _ := yaml.Marshal(r)
	return string(b)
}

// List reads every recipe of dir (of one project unless project is ""),
// sorted by name; files that do not parse are returned as errors.
func List(dir, project string) ([]*Recipe, []error) {
	paths, _ := filepath.Glob(filepath.Join(dir, "*.yaml"))
	sort.Strings(paths)
	var out []*Recipe
	var errs []error
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		r, err := Parse(b)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", filepath.Base(p), err))
			continue
		}
		if project == "" || r.Project == project {
			out = append(out, r)
		}
	}
	return out, errs
}

// Load reads one recipe by name.
func Load(dir, name string) (*Recipe, error) {
	if !nameRe.MatchString(name) {
		return nil, fmt.Errorf("không có quy trình %q", name)
	}
	b, err := os.ReadFile(Path(dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("không có quy trình %q", name)
	}
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Path is the file of a recipe.
func Path(dir, name string) string { return filepath.Join(dir, name+".yaml") }

// Save writes a validated recipe.
func Save(dir string, r *Recipe, known func(string) bool) error {
	if err := Validate(r, known); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := Path(dir, r.Name) + ".tmp"
	if err := os.WriteFile(tmp, []byte(Marshal(r)), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, Path(dir, r.Name))
}

// Remove deletes a recipe.
func Remove(dir, name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("không có quy trình %q", name)
	}
	return os.Remove(Path(dir, name))
}
