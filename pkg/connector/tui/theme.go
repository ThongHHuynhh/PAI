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
// near-neutral screen with a trace of the same hue. In every theme, text
// reaches 7:1 on the screen and on the selection 4.5:1, muted text 4.5:1 on
// the panel, and the accent and error 3:1 on the screen (WCAG).
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

// NamedTheme is a theme a person can pick with /theme, by its id.
type NamedTheme struct {
	ID, Label string
	Theme     Theme
}

// themeAuto is the choice that follows the terminal: PAI dark on a dark
// background, PAI light on a light one.
const themeAuto = "auto"

// Themes are the themes /theme lists, in its order. The well-known ones keep
// their palettes' own hues, nudged where a hue missed the contrasts above.
var Themes = []NamedTheme{
	{"pai-dark", "PAI dark", dark},
	{"pai-light", "PAI light", light},
	{"white", "White", Theme{
		BG: term.Hex("#ffffff"), Panel: term.Hex("#f0eff5"),
		Text: term.Hex("#111018"), Muted: term.Hex("#55516a"), Accent: term.Hex("#5b3fd6"),
		Selection: term.Hex("#e2dbfd"), Error: term.Hex("#b42318"),
	}},
	{"high-contrast", "High contrast", Theme{
		BG: term.Hex("#000000"), Panel: term.Hex("#1c1c1c"),
		Text: term.Hex("#ffffff"), Muted: term.Hex("#d4d4d4"), Accent: term.Hex("#ffd400"),
		Selection: term.Hex("#00458a"), Error: term.Hex("#ff7070"),
	}},
	{"nord", "Nord", Theme{
		BG: term.Hex("#2e3440"), Panel: term.Hex("#3b4252"),
		Text: term.Hex("#eceff4"), Muted: term.Hex("#b8c0cf"), Accent: term.Hex("#88c0d0"),
		Selection: term.Hex("#4c566a"), Error: term.Hex("#e0868f"),
	}},
	{"dracula", "Dracula", Theme{
		BG: term.Hex("#282a36"), Panel: term.Hex("#343746"),
		Text: term.Hex("#f8f8f2"), Muted: term.Hex("#b6b9d2"), Accent: term.Hex("#bd93f9"),
		Selection: term.Hex("#44475a"), Error: term.Hex("#ff6e6e"),
	}},
	{"solarized-dark", "Solarized dark", Theme{
		BG: term.Hex("#002b36"), Panel: term.Hex("#073642"),
		Text: term.Hex("#eee8d5"), Muted: term.Hex("#a3b1b1"), Accent: term.Hex("#2aa198"),
		Selection: term.Hex("#0a4b5a"), Error: term.Hex("#ef5350"),
	}},
	{"solarized-light", "Solarized light", Theme{
		BG: term.Hex("#fdf6e3"), Panel: term.Hex("#eee8d5"),
		Text: term.Hex("#073642"), Muted: term.Hex("#4f6168"), Accent: term.Hex("#1e6fae"),
		Selection: term.Hex("#e3dcc4"), Error: term.Hex("#c2271f"),
	}},
	{"gruvbox", "Gruvbox", Theme{
		BG: term.Hex("#282828"), Panel: term.Hex("#3c3836"),
		Text: term.Hex("#ebdbb2"), Muted: term.Hex("#bdae93"), Accent: term.Hex("#fabd2f"),
		Selection: term.Hex("#504945"), Error: term.Hex("#fb4934"),
	}},
	{"catppuccin-mocha", "Catppuccin Mocha", Theme{
		BG: term.Hex("#1e1e2e"), Panel: term.Hex("#313244"),
		Text: term.Hex("#cdd6f4"), Muted: term.Hex("#a6adc8"), Accent: term.Hex("#cba6f7"),
		Selection: term.Hex("#45475a"), Error: term.Hex("#f38ba8"),
	}},
	{"catppuccin-latte", "Catppuccin Latte", Theme{
		BG: term.Hex("#eff1f5"), Panel: term.Hex("#dce0e8"),
		Text: term.Hex("#4c4f69"), Muted: term.Hex("#5c5f77"), Accent: term.Hex("#8839ef"),
		Selection: term.Hex("#ccd0da"), Error: term.Hex("#d20f39"),
	}},
	{"tokyo-night", "Tokyo Night", Theme{
		BG: term.Hex("#1a1b26"), Panel: term.Hex("#24283b"),
		Text: term.Hex("#c0caf5"), Muted: term.Hex("#a9b1d6"), Accent: term.Hex("#7aa2f7"),
		Selection: term.Hex("#33467c"), Error: term.Hex("#f7768e"),
	}},
}

// themeFor is theme id for the terminal; auto, or an id no theme has, follows
// the terminal's background. With 256 colors the panel keeps a shade apart
// from the screen.
func themeFor(c term.Caps, id string) Theme {
	t := light
	if c.Dark {
		t = dark
	}
	for _, n := range Themes {
		if n.ID == id {
			t = n.Theme
		}
	}
	if !c.Truecolor {
		t.Panel = term.Apart256(t.Panel, t.BG)
	}
	return t
}

func (t Theme) text() term.Style  { return term.Style{FG: t.Text} }
func (t Theme) muted() term.Style { return term.Style{FG: t.Muted} }

// Ink is a theme color a view names instead of holding, so it follows the
// theme when the person switches it.
type Ink uint8

const (
	InkNone Ink = iota
	InkText
	InkMuted
	InkAccent
	InkError
)

// Paint is the theme color a node fills its rect with, named the same way.
type Paint uint8

const (
	PaintNone Paint = iota
	PaintScreen
	PaintPanel
)

func (t Theme) ink(i Ink) term.Color {
	return [...]term.Color{0, t.Text, t.Muted, t.Accent, t.Error}[i]
}

func (t Theme) paint(p Paint) term.Color {
	return [...]term.Color{0, t.BG, t.Panel}[p]
}

// SetTheme switches to theme id, auto included, at once: every view reads its
// colors at the next draw, and laid out rows, which hold theirs, are laid out
// again.
func (a *App) SetTheme(id string) {
	a.Theme, a.ThemeID = themeFor(a.T.Caps, id), id
	a.themeGen++
}
