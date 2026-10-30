package tui

// themeMenu lists the themes. Moving through it shows each one at once,
// Enter keeps it in ~/.pai/tui.json, and Esc goes back to the one before.
func (s *session) themeMenu(a *App) {
	was := a.ThemeID
	ids := []string{themeAuto}
	m := &Menu{Items: []MenuItem{{Label: "Auto", Hint: "follows the terminal"}}}
	for _, n := range Themes {
		ids = append(ids, n.ID)
		m.Items = append(m.Items, MenuItem{Label: n.Label})
	}
	for i, id := range ids {
		if id == was || i == 0 && (was == "" || was == themeAuto) {
			m.Start = i
			m.Items[i].Hint = "current"
		}
		m.Items[i].Pick = func(a *App, it *MenuItem) {
			a.SetTheme(id)
			saved := id
			if id == themeAuto {
				saved = "" // none saved follows the terminal
			}
			if err := savePrefs(prefs{Theme: saved}); err != nil {
				a.SayError(err)
				return
			}
			a.Say("Theme: " + it.Label)
		}
	}
	m.Hover = func(a *App, it *MenuItem) {
		for i := range m.Items {
			if &m.Items[i] == it {
				a.SetTheme(ids[i])
			}
		}
	}
	a.OpenMenu("Theme", m)
	m.layer.Esc = func() { a.SetTheme(was) }
}

// themeLabel is the name of theme id, as /theme lists it.
func themeLabel(id string) string {
	for _, n := range Themes {
		if n.ID == id {
			return n.Label
		}
	}
	return "Auto"
}
