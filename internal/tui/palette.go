package tui

import "github.com/charmbracelet/lipgloss"

// The palette: one graphite ramp for the chrome, one amber signal for what
// waits on you, and two toned-down semantic colors. Every color in the TUI is
// one of these tokens, each with a dark and a light terminal value.
//
// Amber is reserved: a gate waiting for your decision, a task that needs you,
// a warning you must act on. Selection, focus and links stay neutral.
const (
	cBright  = "bright"  // titles, the selected row, figures that matter
	cText    = "text"    // body text
	cSub     = "sub"     // secondary text, focused borders, the selection bar
	cMuted   = "muted"   // meta, hints, separators
	cRule    = "rule"    // hairlines, idle borders
	cSurface = "surface" // one step up from the background: selection, keycaps
	cAccent  = "accent"  // the amber signal
	cOk      = "ok"
	cErr     = "err"
)

var palette = map[string]lipgloss.AdaptiveColor{
	cBright:  {Dark: "255", Light: "232"},
	cText:    {Dark: "252", Light: "236"},
	cSub:     {Dark: "246", Light: "241"},
	cMuted:   {Dark: "242", Light: "246"},
	cRule:    {Dark: "238", Light: "252"},
	cSurface: {Dark: "236", Light: "254"},
	cAccent:  {Dark: "214", Light: "130"},
	cOk:      {Dark: "108", Light: "29"},
	cErr:     {Dark: "167", Light: "124"},
}

// clr resolves a palette token; anything else is passed through as a raw
// terminal color.
func clr(token string) lipgloss.TerminalColor {
	if c, ok := palette[token]; ok {
		return c
	}
	return lipgloss.Color(token)
}

// fg is a style in one palette color.
func fg(token string) lipgloss.Style { return lipgloss.NewStyle().Foreground(clr(token)) }
