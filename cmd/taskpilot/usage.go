package main

import (
	"fmt"
	"os"

	"github.com/buonnguwaaa/taskpilot/internal/oneshot"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

// recordUsage stores the token usage of every helper `claude -p` call this
// process makes (the Usage tab).
func recordUsage(st *store.Store) {
	oneshot.Record = func(u oneshot.Usage) {
		err := st.AddClaudeCall(store.ClaudeCall{At: u.At, Purpose: u.Purpose, Model: u.Model,
			Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, CostUSD: u.CostUSD})
		if err != nil {
			fmt.Fprintln(os.Stderr, "taskpilot: ghi usage:", err)
		}
	}
}
