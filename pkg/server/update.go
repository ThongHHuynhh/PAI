package server

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"pai/pkg/release"
)

// Update makes the installed harness release v, the newest stable release
// when v is empty, prints what the changelog says changed, and restarts it.
// Workers follow at their next hello, so one command updates the fleet. The
// files come through the running harness's /releases when it answers, so the
// source its config names applies; else from the place built in, as before.
// An older release is refused unless downgrade: a mistyped version, a
// stale latest, or a source that lies about latest cannot roll the fleet back
// by accident; workers still follow whatever the server runs.
func Update(v string, downgrade bool) error {
	out, err := exec.Command(bin, "version").Output()
	if err != nil {
		return fmt.Errorf("no installed harness at %s: %w", bin, err)
	}
	cur := string(bytes.TrimSpace(out))
	local := localReleases()
	named := v != ""
	if !named {
		if v, err = latest(local); err != nil {
			return err
		}
	}
	if skip, err := refuseDowngrade(v, cur, named, downgrade); skip || err != nil {
		return err
	}
	log, err := release.Get(v, "CHANGELOG.md", local)
	if err != nil {
		return err
	}
	if err := release.Install(bin, "pai-server", v, local); err != nil {
		return err
	}
	fmt.Printf("pai-server %s -> %s\n\n%s", cur, v, notes(string(log), cur))
	return restart()
}

// localReleases is the installed harness's /releases on its loopback
// address, or "" with no installed config.
func localReleases() string {
	c, err := Load(Conf)
	if err != nil || c.HTTP == "" {
		return ""
	}
	return "http://" + c.HTTP + "/releases"
}

// latest is the newest stable release, as the running harness's source has
// it, else as the place built in has it.
func latest(local string) (string, error) {
	if local != "" {
		if v, err := (release.Source{URL: local}).Latest(context.Background()); err == nil {
			return v, nil
		}
	}
	return release.Latest()
}

// notes is the changelog's sections above release cur's, and cur's own when
// cur is a candidate for it. A changelog without cur's section, from an older
// release or against a local build, gives none.
func notes(log, cur string) string {
	base, _, rc := strings.Cut(cur, "-")
	end := strings.Index(log, "\n## "+base+"\n") + 1
	if end == 0 {
		return ""
	}
	if rc {
		if j := strings.Index(log[end:], "\n## "); j >= 0 {
			end += j + 1
		} else {
			end = len(log)
		}
	}
	return log[strings.Index(log, "\n## ")+1 : end]
}

// refuseDowngrade is whether updating from cur to v is nothing to do (skip),
// saying why: v is cur, or v, the latest stable, is older than a beta this
// server runs; or a refusal, when v, named, is older and downgrade was not
// given.
func refuseDowngrade(v, cur string, named, downgrade bool) (skip bool, err error) {
	switch {
	case v == cur:
		fmt.Printf("pai-server is %s\n", v)
		return true, nil
	case !named && release.Older(v, cur):
		fmt.Printf("pai-server is %s, newer than the latest stable release %s: nothing to do\n", cur, v)
		return true, nil
	case release.Older(v, cur) && !downgrade:
		return false, fmt.Errorf("%s is older than this server's %s: to roll back, pai-server update %s --allow-downgrade", v, cur, v)
	}
	return false, nil
}
