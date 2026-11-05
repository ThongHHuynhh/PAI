package term

import "testing"

func TestBackgroundEnv(t *testing.T) {
	for _, c := range []struct {
		env        string
		dark, want bool
	}{
		{"light", true, false},
		{"dark", false, true},
		{"", true, true},
		{"", false, false},
		{"auto", false, false},
		{"Light", true, true},
	} {
		t.Setenv(themeEnv, c.env)
		if got := background(c.dark); got != c.want {
			t.Errorf("%s=%q on dark %v: dark %v, want %v", themeEnv, c.env, c.dark, got, c.want)
		}
	}
}
