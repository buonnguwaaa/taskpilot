package main

import (
	"fmt"
	"strings"

	"github.com/buonnguwaaa/taskpilot/internal/store"
)

const grantsUsage = `taskpilot grants [list]                      the commands you allowed while tasks ran (request_permission)
taskpilot grants rm task|project TARGET RULE  take one back, e.g. grants rm project bom 'Bash(curl *)'
To allow a command for good, add it to allowed_tools in the config instead.
`

func cmdGrants(args []string) error {
	sub := "list"
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	c := newFlags("grants " + sub)
	pos := c.parse(args)
	_, st, err := c.load()
	if err != nil {
		return err
	}
	switch sub {
	case "list":
		gs, err := st.AllGrants()
		if err != nil {
			return err
		}
		if len(gs) == 0 {
			fmt.Println("no grants (agents ask with request_permission; you answer in the TUI)")
			return nil
		}
		for _, g := range gs {
			fmt.Printf("%-8s %-28s %-30s %s\n", g.Scope, g.Target, g.Rule, g.At.Local().Format("02/01 15:04"))
		}
		return nil
	case "rm":
		if len(pos) != 3 || pos[0] != store.GrantTask && pos[0] != store.GrantProject {
			fmt.Print(grantsUsage)
			return fmt.Errorf("usage: grants rm task|project TARGET RULE")
		}
		if err := st.RemoveGrant(pos[0], pos[1], strings.TrimSpace(pos[2])); err != nil {
			return err
		}
		fmt.Println("removed", pos[2], "from", pos[0], pos[1])
		return nil
	}
	fmt.Print(grantsUsage)
	return nil
}
