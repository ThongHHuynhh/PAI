package tui

import (
	"math"
	"testing"

	"pai/pkg/connector/tui/term"
)

// contrast is the WCAG contrast ratio of two colors.
func contrast(a, b term.Color) float64 {
	lum := func(c term.Color) float64 {
		ch := func(v uint8) float64 {
			x := float64(v) / 255
			if x <= 0.03928 {
				return x / 12.92
			}
			return math.Pow((x+0.055)/1.055, 2.4)
		}
		return 0.2126*ch(uint8(c>>16)) + 0.7152*ch(uint8(c>>8)) + 0.0722*ch(uint8(c))
	}
	l1, l2 := lum(a), lum(b)
	return (max(l1, l2) + 0.05) / (min(l1, l2) + 0.05)
}

// Every theme reads: the contrasts theme.go promises.
func TestThemesContrast(t *testing.T) {
	seen := map[string]bool{}
	for _, n := range Themes {
		if seen[n.ID] || n.ID == themeAuto {
			t.Errorf("theme id %q twice, or auto", n.ID)
		}
		seen[n.ID] = true
		th := n.Theme
		for _, c := range []struct {
			what string
			fg   term.Color
			bg   term.Color
			min  float64
		}{
			{"text on screen", th.Text, th.BG, 7},
			{"text on selection", th.Text, th.Selection, 4.5},
			{"muted on panel", th.Muted, th.Panel, 4.5},
			{"accent on screen", th.Accent, th.BG, 3},
			{"error on screen", th.Error, th.BG, 3},
		} {
			if r := contrast(c.fg, c.bg); r < c.min {
				t.Errorf("%s: %s is %.2f:1, want %.1f:1", n.ID, c.what, r, c.min)
			}
		}
	}
}

func TestThemeFor(t *testing.T) {
	dark256 := term.Caps{Dark: true, Truecolor: true}
	if got := themeFor(dark256, ""); got != dark {
		t.Error("no choice on a dark terminal is not PAI dark")
	}
	if got := themeFor(term.Caps{Truecolor: true}, themeAuto); got != light {
		t.Error("auto on a light terminal is not PAI light")
	}
	if got := themeFor(dark256, "no-such-theme"); got != dark {
		t.Error("an unknown id does not follow the terminal")
	}
	if got := themeFor(dark256, "white"); got.BG != term.Hex("#ffffff") {
		t.Error("white on a dark terminal is not white")
	}
}

func TestPrefs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if p := loadPrefs(); p.Theme != "" {
		t.Fatalf("no file loads %q", p.Theme)
	}
	if err := savePrefs(prefs{Theme: "nord"}); err != nil {
		t.Fatal(err)
	}
	if p := loadPrefs(); p.Theme != "nord" {
		t.Fatalf("saved nord, loaded %q", p.Theme)
	}
}
