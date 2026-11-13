// Package scan groups a project's production errors at scheduled times and
// keeps them as fix suggestions for the operator (no task is created by
// itself: the operator decides in the TUI).
package scan

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/splunk"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

// groupsLimit caps the groups one scan reads.
const groupsLimit = 200

// Scanner runs the scheduled scans.
type Scanner struct {
	Config func() *config.Config // the current config (it is reloaded)
	Store  *store.Store
	Splunk func(ctx context.Context) (*splunk.Client, error)
	Logf   func(format string, args ...any)
	Now    func() time.Time
	// Group joins the groups of one failing path that share no id (pass 2 of
	// the flows); nil: only shared request ids / transaction codes join.
	Group Grouper

	mu      sync.Mutex
	running map[string]bool
}

func (s *Scanner) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Scanner) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// RetryAfter is how long a failed scan waits before trying again.
const RetryAfter = 15 * time.Minute

// Due reports whether the project's latest scheduled time has passed since
// its last scan (missed times are not replayed: one scan catches up). A
// failed scan is retried after RetryAfter.
func Due(sc *config.ErrorScan, last *store.ErrorScan, now time.Time) bool {
	if last != nil && last.Error != "" {
		return now.Sub(last.StartedAt) >= RetryAfter
	}
	times, err := sc.ScanTimes()
	if err != nil || len(times) == 0 {
		return false
	}
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	var slot time.Time
	for _, m := range times {
		if t := day.Add(time.Duration(m) * time.Minute); !t.After(now) {
			slot = t
		}
	}
	if slot.IsZero() { // before today's first time: yesterday's last one
		slot = day.Add(-24 * time.Hour).Add(time.Duration(times[len(times)-1]) * time.Minute)
	}
	return last == nil || last.StartedAt.Before(slot)
}

// Window is what a scan reads: since the previous scan, at most MaxWindow.
func Window(sc *config.ErrorScan, last *store.ErrorScan, now time.Time) (time.Time, time.Time) {
	from := now.Add(-sc.MaxWindow)
	if last != nil && last.Latest.After(from) {
		from = last.Latest
	}
	return from, now
}

// Tick runs the scans that are due (each project at most once at a time).
func (s *Scanner) Tick(ctx context.Context) {
	cfg := s.Config()
	for _, name := range cfg.ProjectNames() {
		p := cfg.Projects[name]
		if p.LogsProduction == nil || p.LogsProduction.Scan == nil {
			continue
		}
		last, err := s.Store.LastScan(name)
		if err != nil || !Due(p.LogsProduction.Scan, last, s.now()) {
			continue
		}
		if _, err := s.Run(ctx, name); err != nil {
			s.logf("scan %s: %v", name, err)
		}
	}
}

// Run scans one project now; it returns how many suggestions are new.
func (s *Scanner) Run(ctx context.Context, project string) (int, error) {
	s.mu.Lock()
	if s.running == nil {
		s.running = map[string]bool{}
	}
	if s.running[project] {
		s.mu.Unlock()
		return 0, fmt.Errorf("a scan of %s is already running", project)
	}
	s.running[project] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.running, project)
		s.mu.Unlock()
	}()

	p, ok := s.Config().Projects[project]
	if !ok || p.LogsProduction == nil || p.LogsProduction.Scan == nil {
		return 0, fmt.Errorf("project %s has no logs_production.scan", project)
	}
	pl, sc := p.LogsProduction, p.LogsProduction.Scan
	// The window starts where the last successful scan ended: a failed scan
	// must not leave a hole.
	last, err := s.Store.LastGoodScan(project)
	if err != nil {
		return 0, err
	}
	now := s.now()
	from, to := Window(sc, last, now)
	id, err := s.Store.StartScan(project, from, to)
	if err != nil {
		return 0, err
	}
	groups, err := s.search(ctx, pl, from, to)
	if err != nil {
		_ = s.Store.FinishScan(id, 0, 0, err)
		return 0, err
	}
	kept, fresh := 0, 0
	var keptGroups []splunk.ErrorGroup
	var ids []int64
	for _, g := range groups {
		if !keep(sc, g) {
			continue
		}
		kept++
		sg := &store.Suggestion{Project: project, Service: g.Service, Signature: g.Signature,
			LastCount: g.Count, FirstSeen: g.First, LastSeen: g.Last, Sample: g.Sample, Caller: g.Caller, Prefix: g.Prefix,
			RequestID: g.RequestID, Stacktrace: g.Stacktrace}
		isNew, err := s.Store.UpsertSuggestion(sg, id, now)
		if err != nil {
			_ = s.Store.FinishScan(id, kept, fresh, err)
			return fresh, err
		}
		if isNew {
			fresh++
		}
		keptGroups, ids = append(keptGroups, g), append(ids, sg.ID)
	}
	flows := s.flows(ctx, project, pl, from, to, keptGroups)
	multi := 0
	for _, f := range flows {
		if len(f.Members) < 2 {
			continue
		}
		multi++
		fids := make([]int64, len(f.Members))
		for i, m := range f.Members {
			fids[i] = ids[m]
		}
		if err := s.Store.SaveFlow(project, id, f.Title, f.Cause, f.How, fids); err != nil {
			s.logf("scan %s: save flow: %v", project, err)
		}
	}
	s.logf("scan %s: %s → %s, %d error group(s) in %d flow(s), %d new suggestion(s)", project, from.Local().Format("02/01 15:04"),
		to.Local().Format("15:04"), kept, len(flows), fresh)
	return fresh, s.Store.FinishScan(id, kept, fresh, nil)
}

func (s *Scanner) search(ctx context.Context, pl *config.ProdLogs, from, to time.Time) ([]splunk.ErrorGroup, error) {
	rows, err := s.query(ctx, pl, splunk.GroupSPL("", groupsLimit), from, to)
	if err != nil {
		return nil, err
	}
	return splunk.ParseGroups(rows), nil
}

func (s *Scanner) query(ctx context.Context, pl *config.ProdLogs, spl string, from, to time.Time) ([]splunk.Event, error) {
	if s.Splunk == nil {
		return nil, splunk.ErrNoCredentials
	}
	c, err := s.Splunk(ctx)
	if err != nil {
		return nil, err
	}
	sctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	// Epoch seconds: an ISO time without zone would be read in the Splunk server's zone.
	return c.Search(sctx, splunk.Request{SPL: splunk.Scope(pl.Filter, spl), Earliest: strconv.FormatInt(from.Unix(), 10),
		Latest: strconv.FormatInt(to.Unix(), 10), MaxEvents: groupsLimit})
}

// flows groups the kept groups into failing paths; a failed pass only
// leaves the groups less grouped.
func (s *Scanner) flows(ctx context.Context, project string, pl *config.ProdLogs, from, to time.Time, groups []splunk.ErrorGroup) []Flow {
	if len(groups) < 2 {
		return buildFlows(groups, nil, nil)
	}
	var links []splunk.Link
	if rows, err := s.query(ctx, pl, splunk.LinkSPL("", groupsLimit), from, to); err != nil {
		s.logf("scan %s: link search: %v", project, err)
	} else {
		links = splunk.ParseLinks(rows)
	}
	proven := linkFlows(groups, links)
	var proposed []Flow
	if s.Group != nil {
		var err error
		proposed, err = s.Group(ctx, GroupInput{Project: project, Language: s.Config().OperatorLanguage, Groups: groups, Linked: proven})
		if err != nil {
			s.logf("scan %s: claude grouping: %v (only shared ids are used)", project, err)
		}
	}
	return buildFlows(groups, proven, proposed)
}

// keep applies the threshold, the services and the ignore list.
func keep(sc *config.ErrorScan, g splunk.ErrorGroup) bool {
	if g.Count < sc.MinCount {
		return false
	}
	if len(sc.Services) > 0 {
		in := false
		for _, s := range sc.Services {
			in = in || s == g.Service
		}
		if !in {
			return false
		}
	}
	text := strings.ToLower(g.Signature + "\n" + g.Sample)
	for _, ig := range sc.Ignore {
		if ig = strings.ToLower(strings.TrimSpace(ig)); ig != "" && strings.Contains(text, ig) {
			return false
		}
	}
	return true
}

// Regroup groups the project's waiting suggestions into flows again (after a
// change of the grouping, or to group suggestions left by older scans). It
// returns the flows with more than one group.
func (s *Scanner) Regroup(ctx context.Context, project string) ([]Flow, []*store.Suggestion, error) {
	p, ok := s.Config().Projects[project]
	if !ok || p.LogsProduction == nil || p.LogsProduction.Scan == nil {
		return nil, nil, fmt.Errorf("project %s has no logs_production.scan", project)
	}
	gs, err := s.Store.Suggestions(project)
	if err != nil {
		return nil, nil, err
	}
	var from, to time.Time
	groups := make([]splunk.ErrorGroup, len(gs))
	for i, g := range gs {
		groups[i] = splunk.ErrorGroup{Service: g.Service, Signature: g.Signature, Count: g.LastCount, First: g.FirstSeen, Last: g.LastSeen,
			Sample: g.Sample, Caller: g.Caller, Prefix: g.Prefix, RequestID: g.RequestID, Stacktrace: g.Stacktrace}
		if from.IsZero() || g.FirstSeen.Before(from) {
			from = g.FirstSeen
		}
		if g.LastSeen.After(to) {
			to = g.LastSeen
		}
	}
	if len(gs) < 2 {
		return nil, gs, nil
	}
	if max := p.LogsProduction.Scan.MaxWindow; to.Sub(from) > max {
		from = to.Add(-max)
	}
	last, _ := s.Store.LastGoodScan(project)
	var scanID int64
	if last != nil {
		scanID = last.ID
	}
	var out []Flow
	for _, f := range s.flows(ctx, project, p.LogsProduction, from.Add(-time.Minute), to.Add(time.Minute), groups) {
		if len(f.Members) < 2 {
			continue
		}
		ids := make([]int64, len(f.Members))
		for i, m := range f.Members {
			ids[i] = gs[m].ID
		}
		if err := s.Store.SaveFlow(project, scanID, f.Title, f.Cause, f.How, ids); err != nil {
			return out, gs, err
		}
		out = append(out, f)
	}
	return out, gs, nil
}
