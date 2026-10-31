package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"pai/pkg/connector"
	"pai/pkg/harness"
)

type accountSettings struct {
	providers []harness.ProviderInfo
	fallbacks map[string]string
	balance   harness.Balance
	accounts  []harness.Account
}

func getAccountSettings(c *connector.Client) (accountSettings, error) {
	ctx, cancel := context.WithTimeout(context.Background(), callTime)
	defer cancel()
	var v accountSettings
	var err error
	if v.providers, err = c.Providers(ctx); err != nil {
		return v, err
	}
	for _, r := range []struct {
		path string
		out  any
	}{
		{"/fallbacks", &v.fallbacks}, {"/balance", &v.balance}, {"/accounts", &v.accounts},
	} {
		if err := c.Call(ctx, "GET", r.path, "", nil, r.out); err != nil {
			return v, err
		}
	}
	if v.fallbacks == nil {
		v.fallbacks = map[string]string{}
	}
	return v, nil
}

func (s *session) saveAccountSetting(a *App, path string, value any) {
	b, err := json.Marshal(value)
	if err != nil {
		a.SayError(err)
		return
	}
	a.Go(func() {
		ctx, cancel := context.WithTimeout(context.Background(), callTime)
		defer cancel()
		err := s.c.Call(ctx, "PUT", path, "application/json", strings.NewReader(string(b)), nil)
		a.Post(func() {
			if err != nil {
				a.SayError(err)
				return
			}
			s.settingsMenu(a)
		})
	})
}

func (s *session) accountRows(m *Menu, v accountSettings) {
	for _, p := range v.providers {
		to := v.fallbacks[p.Name]
		if to == "" {
			to = "None"
		}
		m.Items = append(m.Items, MenuItem{Section: "At usage limit", Label: p.Name, Hint: to, Pick: func(a *App, _ *MenuItem) {
			next := &Menu{}
			set := func(to string) func(*App, *MenuItem) {
				return func(a *App, _ *MenuItem) {
					fb := maps.Clone(v.fallbacks)
					if to == "" {
						delete(fb, p.Name)
					} else {
						fb[p.Name] = to
					}
					s.saveAccountSetting(a, "/fallbacks", fb)
				}
			}
			next.Items = append(next.Items, MenuItem{Label: "None", Pick: set("")})
			for _, target := range v.providers {
				if target.Name != p.Name {
					next.Items = append(next.Items, MenuItem{Label: target.Name, Pick: set(target.Name)})
				}
			}
			next.Items = append(next.Items, MenuItem{Label: "Provider/model…", Pick: func(a *App, _ *MenuItem) {
				ask(a, "Fallback for "+p.Name, "provider/model", func(a *App, text string) { set(text)(a, nil) })
			}})
			a.OpenMenu("At limit: "+p.Name, next)
		}})
	}
	m.Items = append(m.Items, MenuItem{Section: "Balance", Label: "Mode", Hint: v.balance.Mode, Pick: func(a *App, _ *MenuItem) {
		a.OpenMenu("Balance", &Menu{Items: []MenuItem{
			{Label: "Off", Pick: func(a *App, _ *MenuItem) {
				s.saveAccountSetting(a, "/balance", harness.Balance{Mode: "off", Pool: v.balance.Pool})
			}},
			{Label: "Auto: choose interchangeable models", Pick: func(a *App, _ *MenuItem) { s.balancePool(a, v) }},
		}})
	}})
	if len(v.balance.Pool) > 0 {
		m.Items = append(m.Items, MenuItem{Section: "Balance", Label: "Pool", Hint: strings.Join(v.balance.Pool, ", "), Pick: func(a *App, _ *MenuItem) { s.balancePool(a, v) }})
	}
	percent := func(u *float64) string {
		if u == nil {
			return "?"
		}
		return fmt.Sprintf("%.0f%%", *u*100)
	}
	for _, ac := range v.accounts {
		hint := fmt.Sprintf("%s · 5h %s · 7d %s", ac.Status, percent(ac.UtilFiveHour), percent(ac.UtilSevenDay))
		if !ac.LimitedUntil.IsZero() {
			hint += " · until " + ac.LimitedUntil.Local().Format(time.DateTime)
		}
		if !ac.Updated.IsZero() {
			hint += " · seen " + ac.Updated.Local().Format(time.DateTime)
		}
		m.Items = append(m.Items, MenuItem{Section: "Accounts (read only)", Label: ac.Name, Hint: hint})
	}
}

func (s *session) balancePool(a *App, v accountSettings) {
	cat := s.list.cat
	if cat == nil {
		a.Say("The catalog is still loading")
		return
	}
	models := map[string]bool{}
	for _, name := range v.balance.Pool {
		models[name] = true
	}
	for _, p := range v.providers {
		id := p.Name
		if _, known := cat.Providers[id]; !known {
			id = p.Shape
		}
		for _, pr := range cat.Presets {
			if pr.Provider == id {
				models[p.Name+"/"+pr.Model] = true
			}
		}
	}
	m := &Menu{Checks: true}
	for _, model := range slices.Sorted(maps.Keys(models)) {
		m.Items = append(m.Items, MenuItem{Section: "Interchangeable models", Label: model, On: slices.Contains(v.balance.Pool, model)})
	}
	m.Items = append(m.Items, MenuItem{Label: "Add provider/model…", Pick: func(a *App, _ *MenuItem) {
		ask(a, "Add pool member", "provider/model", func(a *App, text string) {
			if text == "" {
				return
			}
			m.Items = append([]MenuItem{{Section: "Interchangeable models", Label: text, On: true}}, m.Items...)
			m.filter()
		})
	}})
	m.Items = append(m.Items, MenuItem{Label: "Save auto mode", Pick: func(a *App, _ *MenuItem) {
		pool := []string{}
		for _, it := range m.Items {
			if it.Section != "" && it.On {
				pool = append(pool, it.Label)
			}
		}
		if len(pool) < 2 {
			a.Say("Choose at least two models")
			return
		}
		a.Pop(m.layer)
		s.saveAccountSetting(a, "/balance", harness.Balance{Mode: "auto", Pool: pool})
	}})
	a.OpenMenu("Balance pool (Space to select)", m)
}
