package worker

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A Windows worker offers pwsh. Where PowerShell 7 is not installed, its runs
// go to Windows PowerShell, with the same wrapping: UTF-8 out, exit code, and
// variables kept between runs.
func TestPwshRunsWhereverPowerShellIs(t *testing.T) {
	it := pwshInterp()
	if _, err := exec.LookPath(it.argv[0]); err != nil {
		t.Skipf("no PowerShell here (%s)", runtime.GOOS)
	}
	e, err := start(it, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer e.kill()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	run := func(code string) (string, int) {
		var out strings.Builder
		exit, alive, err := e.run(ctx, code, "pai-done-test", func(b []byte) error { out.Write(b); return nil })
		if err != nil || !alive {
			t.Fatalf("%q: alive=%v err=%v", code, alive, err)
		}
		return strings.TrimSpace(out.String()), exit
	}
	if out, exit := run("$x = 'ok'; Write-Output \"$x-ü\""); out != "ok-ü" || exit != 0 {
		t.Fatalf("got %q exit %d", out, exit)
	}
	if out, _ := run("Write-Output $x"); out != "ok" {
		t.Fatalf("variable not kept: %q", out)
	}
	if _, exit := run("Get-Item C:\\no\\such\\path -ErrorAction Stop"); exit == 0 {
		t.Fatal("a failure exited 0")
	}
}
