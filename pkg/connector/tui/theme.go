package tui

import "pai/pkg/connector/tui/term"

// Theme is the colors every component takes its look from. Panels are set
// apart by shade, not by lines, and share one shade.
type Theme struct {
	BG        term.Color // the screen
	Panel     term.Color // raised: sidebar, dialogs, the input, the scrollbar track
	Text      term.Color
	Muted     term.Color
	Accent    term.Color
	Selection term.Color
	Error     term.Color
}

// PAI's colors: a violet accent, the hue of the core in the Orbit, on a
// near-neutral screen with a trace of the same hue. Muted text reaches WCAG
// AA, 4.5:1, on the panel in both themes.
var (
	dark = Theme{
		BG: term.Hex("#07060b"), Panel: term.Hex("#121019"),
		Text: term.Hex("#f5f3fa"), Muted: term.Hex("#9a94ab"), Accent: term.Hex("#9585f8"),
		Selection: term.Hex("#2e2650"), Error: term.Hex("#ef7a7f"),
	}
	light = Theme{
		BG: term.Hex("#f8f7fb"), Panel: term.Hex("#e6e3ef"),
		Text: term.Hex("#1b1726"), Muted: term.Hex("#5a546a"), Accent: term.Hex("#6a4fe0"),
		Selection: term.Hex("#ddd5fb"), Error: term.Hex("#c4363a"),
	}
)

// themeFor is the theme for the terminal. With 256 colors the panel keeps a
// shade apart from the screen.
func themeFor(c term.Caps) Theme {
	t := light
	if c.Dark {
		t = dark
	}
	if !c.Truecolor {
		t.Panel = term.Apart256(t.Panel, t.BG)
	}
	return t
}

func (t Theme) text() term.Style  { return term.Style{FG: t.Text} }
func (t Theme) muted() term.Style { return term.Style{FG: t.Muted} }
