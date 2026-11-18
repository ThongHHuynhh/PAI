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
func Update(v string) error {
	out, err := exec.Command(bin, "version").Output()
	if err != nil {
		return fmt.Errorf("no installed harness at %s: %w", bin, err)
	}
	cur := string(bytes.TrimSpace(out))
	local := localReleases()
	if v == "" {
		if v, err = latest(local); err != nil {
			return err
		}
	}
	if v == cur {
		fmt.Printf("pai-server is %s\n", v)
		return nil
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
