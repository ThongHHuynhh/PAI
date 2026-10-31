package tui

import "testing"

func TestMenuDisabledItemCannotBePicked(t *testing.T) {
	picked := false
	m := &Menu{Checks: true, Items: []MenuItem{{Label: "unavailable", Disabled: true, Pick: func(*App, *MenuItem) { picked = true }}}}
	m.filter()
	m.pick(&App{}, m.sel)
	if picked || m.Items[0].On {
		t.Fatal("disabled item was picked or toggled")
	}
}
