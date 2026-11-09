package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

func cmdMigrateFiles(args []string) error {
	c := newFlags("migrate-files")
	c.fs.Parse(args)
	_, st, err := c.load()
	if err != nil {
		return err
	}
	if err := refuseOldDaemon(st.Home); err != nil {
		return err
	}
	ids, err := st.ImportLegacy()
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		fmt.Println("không có task dạng file nào cần chuyển")
		return nil
	}
	fmt.Printf("đã chuyển %d task vào database: %s\n(file cũ nằm trong tasks/<id>/legacy/)\n", len(ids), strings.Join(ids, ", "))
	return nil
}

const dbUsage = `taskpilot db stats            cost, approval rate per gate, top denials, outbox
taskpilot db tasks            tasks with state, runs and cost
taskpilot db runs TASK        runs of a task (trigger, cost, turns, outcome, tool calls)
taskpilot db sql "SELECT …"   any read-only SQL (tables: tasks, task_transitions, approvals, runs,
                              tool_calls, completions, operator_messages, baselines, pull_requests,
                              splunk_queries, outbox; views: v_inbox, v_gate_stats, v_denials, v_cost_daily)
`

func cmdDB(args []string) error {
	if len(args) == 0 {
		fmt.Print(dbUsage)
		return nil
	}
	sub, rest := args[0], args[1:]
	c := newFlags("db " + sub)
	arg := ""
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		arg, rest = rest[0], rest[1:]
	}
	c.fs.Parse(rest)
	_, st, err := c.load()
	if err != nil {
		return err
	}
	switch sub {
	case "stats":
		s, err := st.Stats()
		if err != nil {
			return err
		}
		fmt.Printf("chi phí hôm nay $%.2f · 7 ngày $%.2f (%d lượt chạy) · %d task xong trong 7 ngày\n", s.CostToday, s.Cost7d, s.Runs7d, s.TasksDone7d)
		for _, g := range s.Gates {
			rate := "-"
			if g.ApprovalRate >= 0 {
				rate = fmt.Sprintf("%.0f%%", g.ApprovalRate*100)
			}
			fmt.Printf("cổng %-6s %d yêu cầu · duyệt %d · từ chối %d · đang chờ %d · tỷ lệ duyệt %s · chờ TB %.0f phút\n",
				g.Gate, g.Requests, g.Approved, g.Rejected, g.Pending, rate, g.AvgWaitMinutes)
		}
		if len(s.TopDenials) > 0 {
			fmt.Println("bị chặn nhiều nhất:", strings.Join(s.TopDenials, ", "))
		}
		fmt.Printf("outbox: %d đang chờ gửi, %d gửi thất bại\n", s.OutboxPending, s.OutboxFailed)
		return nil
	case "tasks":
		return runSQL(st.DB.DB, `SELECT id, state, project, runs_count AS runs, printf('$%.2f', cost_usd) AS cost, substr(updated_at,1,16) AS updated, title FROM tasks ORDER BY created_at DESC`)
	case "runs":
		if arg == "" {
			return errors.New("usage: taskpilot db runs TASK")
		}
		runs, err := st.Runs(arg)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "N\tTRIGGER\tSTARTED\tDURATION\tCOST\tTURNS\tTOOLS\tDENIED\tOUTCOME")
		for _, r := range runs {
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t$%.2f\t%d\t%d\t%d\t%s\n", r.N, r.Trigger, r.StartedAt.Local().Format("01-02 15:04"),
				r.Duration.Round(time.Second), r.CostUSD, r.NumTurns, r.ToolCalls, r.Denied, r.Outcome)
		}
		return w.Flush()
	case "sql":
		if arg == "" {
			return errors.New(`usage: taskpilot db sql "SELECT …"`)
		}
		return runSQL(st.DB.DB, arg)
	}
	fmt.Print(dbUsage)
	return fmt.Errorf("unknown db command %q", sub)
}

type sqlDB = *sql.DB

// runSQL prints the result of a read-only query as a table. The connection is
// switched to query_only so a statement cannot change anything.
func runSQL(db sqlDB, query string) error {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "PRAGMA query_only = ON"); err != nil {
		return err
	}
	defer conn.ExecContext(ctx, "PRAGMA query_only = OFF")
	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, strings.ToUpper(strings.Join(cols, "\t")))
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	n := 0
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		cells := make([]string, len(cols))
		for i, v := range vals {
			switch x := v.(type) {
			case nil:
				cells[i] = "NULL"
			case []byte:
				cells[i] = string(x)
			default:
				cells[i] = fmt.Sprint(x)
			}
			cells[i] = short(strings.ReplaceAll(cells[i], "\n", " "), 80)
		}
		fmt.Fprintln(w, strings.Join(cells, "\t"))
		n++
	}
	w.Flush()
	fmt.Printf("(%d dòng)\n", n)
	return rows.Err()
}
