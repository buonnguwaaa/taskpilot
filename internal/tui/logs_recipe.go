package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/buonnguwaaa/taskpilot/internal/recipe"
	"github.com/buonnguwaaa/taskpilot/internal/splunk"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

// Debugging recipes (internal/recipe): the lookups the operator repeats,
// run here without Claude, and the proposals of reflect (key R) waiting for
// the operator. Enter runs (a proposal too, to try it), a accepts, x
// rejects, e edits the YAML, d deletes a recipe.

const itemRecipes = "Quy trình debug"

const recipeKeys = "↑↓ chọn · Enter chạy (đề xuất: chạy thử) · a chấp nhận · x bỏ đề xuất · e sửa YAML · d xoá · R reflect · esc về danh sách"

// recState is the recipe view of the Logs tab.
type recState struct {
	focus      bool
	cursor     int
	recipes    []*recipe.Recipe
	proposals  []*store.RecipeProposal
	parsed     map[int64]*recipe.Recipe // the proposals' recipes
	broken     []error                  // recipe files that do not parse
	running    string                   // recipe being run, "" when none
	reflecting string                   // project being reflected on
	// shown is the last run, while its results fill the pane (n, c).
	shown *recipeRun
}

type recipeRun struct {
	r    *recipe.Recipe
	vals map[string]string
	out  string
}

// recEntry is one row: a waiting proposal or a recipe.
type recEntry struct {
	prop *store.RecipeProposal
	r    *recipe.Recipe
}

func (m Model) recEntries() []recEntry {
	var out []recEntry
	for _, p := range m.logs.rec.proposals {
		if r := m.logs.rec.parsed[p.ID]; r != nil {
			out = append(out, recEntry{p, r})
		}
	}
	for _, r := range m.logs.rec.recipes {
		out = append(out, recEntry{r: r})
	}
	return out
}

func (m Model) recCurrent() *recEntry {
	es := m.recEntries()
	if c := m.logs.rec.cursor; c >= 0 && c < len(es) {
		return &es[c]
	}
	return nil
}

func (m Model) recipeDir() string { return recipe.Dir(m.d.Store.Home) }

func (m Model) knownProject(p string) bool { _, ok := m.d.Config.Projects[p]; return ok }

// recipeCounts are the project's recipes and waiting proposals (list label).
func (m Model) recipeCounts(project string) (int, int) {
	rs, _ := recipe.List(m.recipeDir(), project)
	ps, _ := m.d.Store.RecipeProposals(project, "new")
	return len(rs), len(ps)
}

// loadRecipes reads the project's recipes and waiting proposals.
func (m *Model) loadRecipes() {
	name, _ := m.logProject()
	s := &m.logs.rec
	s.recipes, s.broken = recipe.List(m.recipeDir(), name)
	s.proposals, _ = m.d.Store.RecipeProposals(name, "new")
	s.parsed = map[int64]*recipe.Recipe{}
	for _, p := range s.proposals {
		if r, err := recipe.Parse([]byte(p.YAML)); err == nil {
			r.Project = p.Project
			s.parsed[p.ID] = r
		}
	}
	s.cursor = min(s.cursor, max(0, len(m.recEntries())-1))
}

// openRecipes shows the project's recipes with the cursor in them.
func (m Model) openRecipes() (tea.Model, tea.Cmd) {
	m.logs.rec.cursor = 0
	m.loadRecipes()
	m.logs.rec.focus = true
	m.logs.rec.shown = nil
	m.logs.groupsFocus, m.logs.suggFocus = false, false
	m.renderRecipes()
	m.logs.vp.GotoTop()
	return m, nil
}

func paramNames(r *recipe.Recipe) string {
	var ps []string
	for _, p := range r.Params {
		ps = append(ps, p.Name)
	}
	return strings.Join(ps, ", ")
}

func (m *Model) renderRecipes() {
	w := max(30, m.logs.vp.Width)
	name, _ := m.logProject()
	s := &m.logs.rec
	var b strings.Builder
	b.WriteString(titleStyle.Render("Quy trình debug — "+name) + "\n")
	info := fmt.Sprintf("%d quy trình · %d đề xuất mới", len(s.recipes), len(s.proposals))
	if last, _ := m.d.Store.LastReflect(name); last != nil {
		info += fmt.Sprintf(" · reflect lần cuối %s (%d task, $%.2f)", last.At.Local().Format("02/01 15:04"), last.Tasks, last.CostUSD)
	}
	b.WriteString(dimStyle.Render(wrap(info, w)) + "\n")
	b.WriteString(dimStyle.Render(wrap(recipeKeys, w)) + "\n\n")
	if s.reflecting == name {
		b.WriteString(accentStyle.Render("Claude đang đọc các task gần đây để đề xuất quy trình…") + "\n\n")
	}
	if s.running != "" {
		b.WriteString(accentStyle.Render("Đang chạy "+s.running+"…") + "\n\n")
	}
	for _, e := range s.broken {
		b.WriteString(warnStyle.Render(wrap("file lỗi: "+e.Error(), w)) + "\n")
	}
	es := m.recEntries()
	if len(es) == 0 {
		b.WriteString(dimStyle.Render(wrap("Chưa có quy trình nào. Bấm R để Claude đọc các task điều tra gần đây và đề xuất (khoảng $0,5–1), "+
			"hoặc viết tay một file YAML rồi chạy: taskpilot recipe add FILE.", w)) + "\n")
	}
	sel := 0
	for i, e := range es {
		if i == 0 && e.prop != nil {
			b.WriteString(section(fmt.Sprintf("Đề xuất mới (%d)", len(s.proposals)), "", w) + "\n")
		}
		if e.prop == nil && (i == 0 || es[i-1].prop != nil) {
			b.WriteString(section(fmt.Sprintf("Quy trình (%d)", len(s.recipes)), "", w) + "\n")
		}
		row := fmt.Sprintf("  %s(%s)  %s", e.r.Name, paramNames(e.r), strings.Join(strings.Fields(e.r.Description), " "))
		if e.prop != nil {
			row = fmt.Sprintf("◆ #%d %s(%s)  %s", e.prop.ID, e.r.Name, paramNames(e.r), strings.Join(strings.Fields(e.prop.Reason), " "))
		}
		row = truncate(row, w)
		switch {
		case i == s.cursor && s.focus:
			sel = strings.Count(b.String(), "\n")
			b.WriteString(selStyle.Render(row) + "\n")
		case e.prop != nil:
			b.WriteString(accentStyle.Render(row) + "\n")
		default:
			b.WriteString(row + "\n")
		}
	}
	if e := m.recCurrent(); e != nil {
		b.WriteString("\n" + accentStyle.Render(strings.Repeat("─", min(w, 40))) + "\n")
		recipeDetail(&b, e, w)
	}
	m.logs.intro = false
	m.logs.vpContent = b.String()
	m.logs.vp.SetContent(m.logs.vpContent)
	if h := m.logs.vp.Height; sel < m.logs.vp.YOffset || sel >= m.logs.vp.YOffset+h-1 {
		m.logs.vp.SetYOffset(max(0, sel-h/3))
	}
}

// recipeDetail describes the selected recipe or proposal.
func recipeDetail(b *strings.Builder, e *recEntry, w int) {
	r := e.r
	b.WriteString(titleStyle.Render(r.Name) + dimStyle.Render(" · "+r.Project) + "\n")
	b.WriteString(wrap(strings.TrimSpace(r.Description), w) + "\n")
	if p := e.prop; p != nil {
		b.WriteString("\n" + accentStyle.Render(wrap("Vì sao: "+p.Reason, w)) + "\n")
		if p.Replaces != "" {
			b.WriteString(warnStyle.Render("Thay cho quy trình: "+p.Replaces) + "\n")
		}
		for _, ev := range p.Evidence {
			b.WriteString(dimStyle.Render(wrap("  • "+ev, w)) + "\n")
		}
	}
	if len(r.Params) > 0 {
		b.WriteString("\n" + titleStyle.Render("Tham số") + "\n")
		for _, p := range r.Params {
			line := "  " + p.Name + ": " + p.Desc
			if p.Example != "" {
				line += dimStyle.Render("  (vd " + p.Example + ")")
			}
			b.WriteString(wrap(line, w) + "\n")
		}
	}
	b.WriteString("\n" + titleStyle.Render("Các bước (chỉ đọc)") + "\n")
	for i, s := range r.Steps {
		title := s.Title
		if title == "" {
			title = s.Kind()
		}
		b.WriteString(fmt.Sprintf("  %d. %s\n", i+1, title))
		for _, l := range strings.Split(wrap(s.Command(), w-5), "\n") {
			b.WriteString("     " + dimStyle.Render(l) + "\n")
		}
	}
	if g := strings.TrimSpace(r.Guide); g != "" {
		b.WriteString("\n" + titleStyle.Render("Cách đọc kết quả") + "\n" + renderMarkdown(g, w-2))
	}
}

// recipeKey handles the keys of the recipe view; ok=false lets a key through.
func (m Model) recipeKey(k tea.KeyMsg) (Model, tea.Cmd, bool) {
	e := m.recCurrent()
	s := &m.logs.rec
	switch k.String() {
	case "j", "down":
		s.cursor = min(s.cursor+1, max(0, len(m.recEntries())-1))
	case "k", "up":
		s.cursor = max(0, s.cursor-1)
	case "esc":
		s.focus = false
	case "r":
		m.loadRecipes()
	case "R":
		return m.confirmReflect(), nil, true
	case "enter":
		if e == nil {
			return m, nil, true
		}
		mm, cmd := m.recipeForm(e.r, e.prop != nil)
		return mm, cmd, true
	case "a":
		if e == nil || e.prop == nil {
			m.msg = "a chấp nhận một đề xuất (dòng ◆)"
			return m, nil, true
		}
		r, err := recipe.Accept(m.d.Store, e.prop.ID, m.knownProject)
		if err != nil {
			m.msg = "chấp nhận: " + err.Error()
		} else {
			m.msg = "đã lưu quy trình " + r.Name + ": agent của " + r.Project + " dùng được từ lượt chạy tới"
		}
		m.loadRecipes()
	case "x":
		if e == nil || e.prop == nil {
			m.msg = "x bỏ một đề xuất (dòng ◆); xoá quy trình đã lưu: d"
			return m, nil, true
		}
		id, name, st := e.prop.ID, e.r.Name, m.d.Store
		m.conf = &confirm{text: fmt.Sprintf("Bỏ đề xuất %s?\nReflect sẽ không đề xuất lại quy trình này.", name), onYes: func() tea.Cmd {
			return func() tea.Msg {
				if err := recipe.Reject(st, id); err != nil {
					return recipesChangedMsg("bỏ đề xuất: " + err.Error())
				}
				return recipesChangedMsg("đã bỏ đề xuất " + name)
			}
		}}
		return m, nil, true
	case "d":
		if e == nil || e.prop != nil {
			m.msg = "d xoá một quy trình đã lưu; bỏ đề xuất: x"
			return m, nil, true
		}
		name, home := e.r.Name, m.d.Store.Home
		m.conf = &confirm{text: fmt.Sprintf("Xoá quy trình %s?\nFile %s bị xoá, agent không còn skill này.", name, recipe.Path(recipe.Dir(home), name)),
			onYes: func() tea.Cmd {
				return func() tea.Msg {
					if err := recipe.Remove(recipe.Dir(home), name); err != nil {
						return recipesChangedMsg("xoá: " + err.Error())
					}
					_ = recipe.SyncSkills(home)
					return recipesChangedMsg("đã xoá quy trình " + name)
				}
			}}
		return m, nil, true
	case "e":
		if e == nil {
			return m, nil, true
		}
		mm, cmd := m.editRecipe(e)
		return mm, cmd, true
	default:
		return m, nil, false
	}
	m.renderRecipes()
	return m, nil, true
}

// recipesChangedMsg reports a change made after a confirmation or an edit.
type recipesChangedMsg string

func (m Model) onRecipesChanged(msg recipesChangedMsg) (tea.Model, tea.Cmd) {
	m.msg = string(msg)
	m.loadRecipes()
	if m.logs.rec.focus {
		m.renderRecipes()
	}
	return m, nil
}

// recipeForm asks for the parameters, then runs the recipe.
func (m Model) recipeForm(r *recipe.Recipe, trial bool) (Model, tea.Cmd) {
	if m.d.RunRecipe == nil {
		m.msg = "không chạy được quy trình từ TUI; dùng: taskpilot recipe run " + r.Name
		return m, nil
	}
	if m.logs.rec.running != "" {
		m.msg = "đang chạy " + m.logs.rec.running + ", đợi xong đã"
		return m, nil
	}
	if len(r.Params) == 0 {
		mm, cmd := m.runRecipe(r, map[string]string{})
		return mm, cmd
	}
	title := "Chạy quy trình " + r.Name
	if trial {
		title = "Chạy thử đề xuất " + r.Name
	}
	f := newForm(title, func(v map[string]string) (tea.Cmd, error) {
		vals := map[string]string{}
		for _, p := range r.Params {
			vals[p.Name] = strings.TrimSpace(v[p.Name])
		}
		if _, err := r.Expand(vals); err != nil {
			return nil, err
		}
		return func() tea.Msg { return recipeStartMsg{r, vals} }, nil
	})
	f.hint = strings.TrimSpace(r.Description) + " · chỉ đọc, không tốn Claude"
	for _, p := range r.Params {
		label := p.Name
		if p.Desc != "" {
			label += ": " + p.Desc
		}
		if p.Example != "" {
			label += " (vd " + p.Example + ")"
		}
		f.addText(p.Name, label, "", true)
	}
	m.modal = f
	return m, f.start()
}

// recipeStartMsg starts a run once the parameter form is closed.
type recipeStartMsg struct {
	r    *recipe.Recipe
	vals map[string]string
}

type recipeDoneMsg struct {
	r    *recipe.Recipe
	vals map[string]string
	out  string
	err  error
}

func (m Model) runRecipe(r *recipe.Recipe, vals map[string]string) (Model, tea.Cmd) {
	m.logs.rec.running = r.Name
	m.msg = "đang chạy quy trình " + r.Name + "…"
	m.renderRecipes()
	run := m.d.RunRecipe
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		out, err := run(ctx, r, vals)
		return recipeDoneMsg{r, vals, out, err}
	}
}

func (m Model) onRecipeDone(msg recipeDoneMsg) (tea.Model, tea.Cmd) {
	m.logs.rec.running = ""
	if msg.err != nil {
		m.msg = "quy trình " + msg.r.Name + ": " + msg.err.Error()
		if m.logs.rec.focus {
			m.renderRecipes()
		}
		return m, nil
	}
	m.msg = "xong quy trình " + msg.r.Name + " · n giao kết quả cho agent điều tra · c copy · Enter trên mục Quy trình để quay lại"
	m.logs.rec.focus = false
	m.logs.rec.shown = &recipeRun{msg.r, msg.vals, msg.out}
	m.logs.groupsFocus, m.logs.suggFocus = false, false
	m.setLogsContent(renderRecipeOut(msg.out, max(30, m.logs.vp.Width)))
	return m, nil
}

// renderRecipeOut styles the plain results of a run.
func renderRecipeOut(out string, w int) string {
	var b strings.Builder
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		switch {
		case strings.HasPrefix(l, "## "):
			b.WriteString("\n" + titleStyle.Render(strings.TrimPrefix(l, "## ")) + "\n")
		case strings.HasPrefix(l, "$ "):
			b.WriteString(dimStyle.Render(wrap(l, w)) + "\n")
		case strings.HasPrefix(l, "LỖI: "):
			b.WriteString(errStyle.Render(wrap(l, w)) + "\n")
		case strings.HasPrefix(l, "Quy trình "):
			b.WriteString(accentStyle.Render(wrap(l, w)) + "\n")
		default:
			b.WriteString(wrap(l, w) + "\n")
		}
	}
	return b.String()
}

// investigateFromRecipe opens an investigation ticket with a run's results.
func (m Model) investigateFromRecipe(run *recipeRun) (tea.Model, tea.Cmd) {
	name, pl := m.logProject()
	if run.r.Project != name {
		name = run.r.Project
	}
	filter := ""
	if pl != nil {
		filter = pl.Filter
	}
	var ids []string
	for _, p := range run.r.Params {
		ids = append(ids, run.vals[p.Name])
	}
	e, l := splunk.AbsoluteWindow("-24h", "", time.Now())
	lines := strings.Split(strings.TrimSpace(run.out), "\n")
	if len(lines) > 120 {
		lines = append(lines[:120], "… (đã cắt; agent chạy lại được bằng run_recipe)")
	}
	in := splunk.Investigation{Project: name, Filter: filter, Earliest: e, Latest: l, IDs: strings.Join(ids, ", "),
		Symptom: fmt.Sprintf("Kết quả quy trình %s (%s), cần điều tra tiếp. Chạy lại: run_recipe %s.", run.r.Name, strings.Join(ids, ", "), run.r.Name),
		Samples: lines}
	return m.openInvestigation(in, "từ quy trình "+run.r.Name)
}

// confirmReflect asks before spending a Claude turn on reflect.
func (m Model) confirmReflect() Model {
	name, _ := m.logProject()
	switch {
	case m.d.Reflect == nil:
		m.msg = "không chạy được reflect từ TUI; dùng: taskpilot reflect -project " + name
		return m
	case m.logs.rec.reflecting != "":
		m.msg = "đang reflect " + m.logs.rec.reflecting + ", đợi xong đã"
		return m
	}
	from := "30 ngày qua"
	if last, _ := m.d.Store.LastReflect(name); last != nil && time.Since(last.At) < recipe.DefaultWindow {
		from = "từ lần reflect trước (" + last.At.Local().Format("02/01 15:04") + ")"
	}
	reflect := m.d.Reflect
	m.conf = &confirm{text: fmt.Sprintf("Reflect %s?\nClaude đọc các task có tra cứu của %s và đề xuất quy trình. Một lượt Claude, khoảng $0,5–1. "+
		"Đề xuất chỉ được dùng khi bạn chấp nhận (a).", name, from),
		onYes: func() tea.Cmd {
			return tea.Batch(func() tea.Msg { return reflectStartMsg(name) }, func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), recipe.ReflectTimeout)
				defer cancel()
				out, err := reflect(ctx, name)
				return reflectDoneMsg{name, out, err}
			})
		}}
	return m
}

type reflectStartMsg string

type reflectDoneMsg struct {
	project, summary string
	err              error
}

func (m Model) onReflectDone(msg reflectDoneMsg) (tea.Model, tea.Cmd) {
	m.logs.rec.reflecting = ""
	if msg.err != nil {
		m.msg = "reflect " + msg.project + ": " + msg.err.Error()
	} else {
		m.msg = strings.Join(strings.Fields(msg.summary), " ")
	}
	if name, _ := m.logProject(); name == msg.project && m.logs.rec.focus {
		m.loadRecipes()
		m.logs.rec.cursor = 0
		m.renderRecipes()
	}
	return m, nil
}

// editRecipe opens the recipe (or proposal) YAML in the operator's editor;
// the result is validated when the editor closes. A draft that does not
// validate is kept, and e opens it again.
func (m Model) editRecipe(e *recEntry) (Model, tea.Cmd) {
	dir := m.recipeDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		m.msg = err.Error()
		return m, nil
	}
	key := "recipe-" + e.r.Name
	var propID int64
	if e.prop != nil {
		key, propID = fmt.Sprintf("proposal-%d", e.prop.ID), e.prop.ID
	}
	draft := filepath.Join(dir, ".edit-"+key+".yaml.draft")
	if _, err := os.Stat(draft); err != nil {
		if err := os.WriteFile(draft, []byte(recipe.Marshal(e.r)), 0o644); err != nil {
			m.msg = err.Error()
			return m, nil
		}
	}
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "nano"
		if _, err := exec.LookPath(editor); err != nil {
			editor = "vi"
		}
	}
	args := append(strings.Fields(editor), draft)
	cmd := exec.Command(args[0], args[1:]...)
	old, st, known, home := e.r.Name, m.d.Store, m.knownProject, m.d.Store.Home
	project := e.r.Project
	return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
		if err != nil {
			return recipesChangedMsg("mở editor: " + err.Error())
		}
		b, err := os.ReadFile(draft)
		if err != nil {
			return recipesChangedMsg(err.Error())
		}
		if propID != 0 {
			err = recipe.EditProposal(st, propID, string(b), known)
		} else {
			err = saveEdited(home, old, project, b, known)
		}
		if err != nil {
			return recipesChangedMsg("YAML chưa hợp lệ, chưa lưu (e để sửa tiếp): " + err.Error())
		}
		_ = os.Remove(draft)
		return recipesChangedMsg("đã lưu thay đổi của " + old)
	})
}

// saveEdited saves an edited recipe (renamed: the old file goes).
func saveEdited(home, old, project string, b []byte, known func(string) bool) error {
	r, err := recipe.Parse(b)
	if err != nil {
		return err
	}
	if r.Project == "" {
		r.Project = project
	}
	dir := recipe.Dir(home)
	if r.Name != old {
		if _, err := os.Stat(recipe.Path(dir, r.Name)); err == nil {
			return errors.New("đã có quy trình tên " + r.Name)
		}
	}
	if err := recipe.Save(dir, r, known); err != nil {
		return err
	}
	if r.Name != old {
		_ = recipe.Remove(dir, old)
	}
	return recipe.SyncSkills(home)
}
