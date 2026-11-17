package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"pai/pkg/connector/tui/term"
	"pai/pkg/ultra/coord"
	"pai/pkg/ultra/ctxsel"
	"pai/pkg/ultra/handoff"
	"pai/pkg/ultra/knowledge"
	"pai/pkg/ultra/project"
	"pai/pkg/ultra/task"
)

type coordOutcome = coord.Outcome

func runtimeNewTask(objective string, checks []task.Criterion, executor string) coord.NewTask {
	return coord.NewTask{Objective: objective, Criteria: checks, Executor: executor}
}

// ultraMgmtKeys routes keys to the list or the detail of a management view.
type ultraMgmtKeys struct {
	u      *ultra
	detail bool
}

func (m ultraMgmtKeys) Draw(a *App, c *Canvas, r Rect) {
	u := m.u
	if m.detail {
		u.detail.Draw(a, c, r)
		return
	}
	l := u.lists[u.view]
	head := u.viewHead()
	if head != "" {
		c.Text(r.X, r.Y, cellCut(head, r.W), a.Theme.muted())
		r = Rect{r.X, r.Y + 1, r.W, max(0, r.H-1)}
	}
	l.Draw(a, c, r)
}

func (m ultraMgmtKeys) Key(a *App, k term.Key) bool {
	u := m.u
	if m.detail {
		switch k.String() {
		case "esc":
			u.showDetail = false
			u.focus = fMain
			a.Focus(u.listNode)
			return true
		}
		if u.detail.Key(a, k) {
			return true
		}
		return u.action(k)
	}
	l := u.lists[u.view]
	if l.Key(a, k) {
		if u.showDetail {
			u.inspect()
		}
		return true
	}
	if l.searching {
		return false
	}
	switch k.String() {
	case "esc":
		if !u.showDetail {
			return false // the screen takes it: back to the Workspace
		}
		u.showDetail = false
		return true
	case "enter":
		u.showDetail = true
		u.inspect()
		if a.W < ultraDetailFrom {
			u.focus = fDetail
			a.Focus(u.detailNode)
		}
		return true
	case "?":
		u.help()
		return true
	case "[":
		u.setView(u.view - 1)
		return true
	case "]":
		u.setView(u.view + 1)
		return true
	}
	return u.action(k)
}

// viewHead is the one line above a list: what it shows and its totals.
func (u *ultra) viewHead() string {
	switch u.view {
	case vSkills:
		return "installed skills · [x] enabled for this project · selected/loaded per task"
	case vInstr:
		t := u.curTask()
		scope := "no current task: whole project"
		if t != nil {
			scope = "for " + t.ID
			if len(t.Paths) > 0 {
				scope += " (" + strings.Join(t.Paths, ", ") + ")"
			}
		}
		return "+ applies   n executor loads it   - not applied · " + scope + " · executor " + u.executor
	case vMemory:
		if u.memAll {
			return "all knowledge, every status (s: live only)"
		}
		return "live knowledge: verified, candidate, stale (s: all)"
	case vRuns:
		return "tasks, newest first"
	case vContext:
		if u.ctxNow == nil {
			return ""
		}
		cx := u.ctxNow
		over := ""
		if cx.Overrun {
			over = " · OVER BUDGET"
		}
		return fmt.Sprintf("~%d tokens (estimate) · budget %d · mandatory ~%d · config %s%s", cx.Tokens, cx.Budget, cx.Mandatory, project.RevisionLabel(cx.ConfigRev), over)
	}
	return ""
}

// --- Skills ---------------------------------------------------------------

func (u *ultra) loadSkills(l *ulist) {
	installed, probs := u.proj.Installed()
	r, rprobs := u.resolved()
	var rows []urow
	seen := map[string]bool{}
	t := u.curTask()
	for _, pk := range installed {
		if pk.Kind != project.KindSkill || seen[pk.Name] {
			continue
		}
		seen[pk.Name] = true
		row := urow{Key: pk.Name, Mark: "[ ]", Label: pk.Name, State: "installed", Hint: fmt.Sprintf("%s · %s · ~%d tok", cmpStr(pk.Meta.VersionOf(), "-"), pk.Library, pk.Tokens),
			Search: strings.Join(pk.Meta.TagsOf(), " ") + " " + pk.Library + " " + pk.Meta.Compatibility + " " + pk.Meta.Description}
		if en, ok := r.Find(project.KindSkill, pk.Name); ok {
			row.Mark = "[x]"
			row.State = "enabled, " + en.Status
			if sel, _ := skillSelected(en, t); sel {
				row.State += ", selected"
			}
			if st := u.loadedStatus("skill:" + pk.Name); strings.HasPrefix(st, "loaded") || st == ctxsel.LoadedOnAsk {
				row.State += ", " + st
			}
			row.Ink = InkText
		}
		rows = append(rows, row)
	}
	for _, en := range r.Enabled {
		if en.Kind == project.KindSkill && !seen[en.Entry.Name] {
			rows = append(rows, urow{Key: en.Entry.Name, Mark: "[!]", Label: en.Entry.Name, State: "enabled, " + en.Status, Ink: InkError})
		}
	}
	l.Empty = "No skills installed. Add one at ~/.pai/ultra/library/skills/<name>/SKILL.md or .pai/library/skills/."
	l.set(rows)
	l.Err = problemsLine(append(probs, rprobs...))
}

func problemsLine(ps []project.Problem) string {
	var errs []string
	for _, p := range ps {
		if p.Severity == "error" {
			errs = append(errs, p.Name+": "+p.Message)
		}
	}
	return strings.Join(errs, "; ")
}

// skillSelected says whether an enabled skill is selected for t's next run, and why.
func skillSelected(en project.Enabled, t *task.Task) (bool, string) {
	act := en.ActivationOf()
	var paths []string
	if t != nil && len(t.Paths) > 0 {
		paths = t.Paths
	}
	switch {
	case act == project.Manual && (t == nil || !slices.Contains(t.Skills, en.Pkg.Name)):
		return false, "manual: the task does not ask for it"
	case len(en.PathsOf()) > 0 && paths != nil && !project.InScope(en.PathsOf(), paths):
		return false, "path scope does not match the task"
	case act == project.Always:
		return true, "always: full instructions"
	}
	return true, "description offered; the file is read when needed"
}

func (u *ultra) skillDetail(name string) string {
	installed, _ := u.proj.Installed()
	r, _ := u.resolved()
	var b strings.Builder
	var pk *project.Package
	if en, ok := r.Find(project.KindSkill, name); ok {
		pk = &en.Pkg
		sel, why := skillSelected(en, u.curTask())
		fmt.Fprintf(&b, "Enabled via %s · %s\nActivation: %s · paths: %s\nSelected for next run: %v (%s)\nLoaded in current context: %s\n",
			en.Via, en.Status, en.ActivationOf(), strings.Join(cmpSlice(en.PathsOf(), []string{"whole project"}), ", "), sel, why, u.loadedStatus("skill:"+name))
		if en.Status == project.StatusUpdate && en.Installed != nil {
			fmt.Fprintf(&b, "Update available: library has %s %s (u to take it)\n%s", en.Installed.Meta.VersionOf(), project.Short(en.Installed.Hash), project.Diff(string(en.Pkg.Raw), string(en.Installed.Raw)))
		}
		if en.Override != nil {
			fmt.Fprintf(&b, "Project override: %s\n", en.Override.Path)
		}
	} else if i := slices.IndexFunc(installed, func(p project.Package) bool { return p.Kind == project.KindSkill && p.Name == name }); i >= 0 {
		pk = &installed[i]
		b.WriteString("Installed, not enabled (Space enables)\n")
	}
	if pk == nil {
		return name + ": not found"
	}
	fmt.Fprintf(&b, "%s %s · %s · %s · ~%d tokens (estimate)\n%s\n", pk.Name, cmpStr(pk.Meta.VersionOf(), "unversioned"), pk.Library, project.Short(pk.Hash), pk.Tokens, pk.Path)
	if t := pk.Meta.TagsOf(); len(t) > 0 {
		b.WriteString("Tags: " + strings.Join(t, ", ") + "\n")
	}
	if pk.Meta.Compatibility != "" {
		b.WriteString("Compatibility: " + pk.Meta.Compatibility + "\n")
	}
	if req := pk.Meta.Requires(); len(req) > 0 {
		b.WriteString("Requires: " + strings.Join(req, ", ") + "\n")
	}
	if rs := pk.Resources(); len(rs) > 0 {
		b.WriteString("Resources: " + strings.Join(rs, ", ") + "\n")
	}
	b.WriteString(strings.Repeat("─", 30) + "\n" + string(pk.Raw))
	return b.String()
}

func cmpSlice(a, b []string) []string {
	if len(a) > 0 {
		return a
	}
	return b
}

// --- Instructions ---------------------------------------------------------

func (u *ultra) selection() project.Selection {
	t := u.curTask()
	var paths, skills []string
	if t != nil {
		if len(t.Paths) > 0 {
			paths = t.Paths
		}
		skills = t.Skills
	}
	r, _ := u.resolved()
	return project.Apply(u.proj.Inventory(r), u.executor, paths, skills)
}

func (u *ultra) loadInstructions(l *ulist) {
	sel := u.selection()
	var rows []urow
	for _, a := range sel.Applied {
		mark := "-"
		switch {
		case a.Include:
			mark = "+"
		case a.Native:
			mark = "n"
		}
		owner := a.Owner
		if a.Editable() {
			owner += ", editable"
		}
		rows = append(rows, urow{Key: a.ID, Mark: mark, Label: a.ID, State: a.Source + " · " + owner, Hint: fmt.Sprintf("~%d tok · %s", a.Tokens, a.Reason)})
	}
	l.Empty = "No instructions. n creates a project rule in .pai/rules/."
	l.set(rows)
	var warn []string
	for _, c := range sel.Unresolved() {
		warn = append(warn, "UNRESOLVED conflict on "+c.Key)
	}
	if len(warn) > 0 {
		l.Err = strings.Join(warn, "; ") + " (c shows conflicts)"
	}
}

func (u *ultra) instructionDetail(id string) string {
	sel := u.selection()
	i := slices.IndexFunc(sel.Applied, func(a project.Applied) bool { return a.ID == id })
	if i < 0 {
		return id + ": not found"
	}
	a := sel.Applied[i]
	order := u.proj.Manifest.Order()
	var b strings.Builder
	fmt.Fprintf(&b, "%s\nSource: %s (precedence %d of %d: %s)\nOwner: %s · editable here: %v\nPath: %s\nRevision: %s · ~%d tokens (estimate)\nActivation: %s · mandatory: %v · paths: %s\n",
		a.ID, a.Source, a.Rank+1, len(order), strings.Join(order, " > "), a.Owner, a.Editable(), a.Path, a.Revision, a.Tokens, a.Activation, a.Mandatory,
		strings.Join(cmpSlice(a.Paths, []string{"whole project"}), ", "))
	fmt.Fprintf(&b, "For this task: %s\n", a.Reason)
	if a.Key != "" {
		fmt.Fprintf(&b, "Declares %s = %s\n", a.Key, a.Value)
	}
	if a.Overrides != "" {
		b.WriteString("Overrides " + a.Overrides + "\n")
	}
	if a.ShadowedBy != "" {
		b.WriteString("Replaced by " + a.ShadowedBy + "\n")
	}
	switch a.Owner {
	case project.OwnerGlobal:
		b.WriteString("Global: read-only here. o makes a project copy.\n")
	case project.OwnerPackage:
		b.WriteString("Package: read-only. o makes a project override; the package is not changed.\n")
	}
	text := a.Text
	if text == "" {
		if raw, err := os.ReadFile(a.Path); err == nil {
			text = string(raw)
		}
	}
	b.WriteString(strings.Repeat("─", 30) + "\n" + text)
	return b.String()
}

func (u *ultra) conflictsText() string {
	sel := u.selection()
	var b strings.Builder
	b.WriteString("Configured checks only: front-matter keys and exact duplicate lines.\nPAI does not judge contradictions in free text; review them yourself.\n\n")
	if len(sel.Conflicts) == 0 {
		b.WriteString("No declared conflicts.\n")
	}
	for _, c := range sel.Conflicts {
		state := "UNRESOLVED (same precedence)"
		if c.Resolved {
			state = "resolved by precedence: " + c.Winner
		}
		fmt.Fprintf(&b, "%s: %s\n", c.Key, state)
		for i, id := range c.IDs {
			fmt.Fprintf(&b, "  %s = %s\n", id, c.Values[i])
		}
	}
	if len(sel.Duplicates) > 0 {
		b.WriteString("\nDuplicate lines:\n")
	}
	for _, d := range sel.Duplicates {
		fmt.Fprintf(&b, "  %q in %s\n", cellCut(d.Line, 70), strings.Join(d.IDs, ", "))
	}
	return b.String()
}

// --- Memory ---------------------------------------------------------------

func (u *ultra) loadMemory(l *ulist) {
	f := knowledge.Filter{Limit: 500}
	if u.memAll {
		f.Statuses = knowledge.AllStatuses
	}
	hits, err := u.rt.Knowledge.Search(context.Background(), "", f)
	if err != nil {
		l.Err = err.Error()
	}
	var rows []urow
	for _, h := range hits {
		eff := h.Effective()
		mark := map[string]string{knowledge.Verified: "✓", knowledge.Candidate: "?", knowledge.Stale: "!", knowledge.Superseded: "→", knowledge.Invalid: "×"}[eff]
		rows = append(rows, urow{Key: h.ID, Mark: mark, Label: cellCut(strings.Join(strings.Fields(h.Text), " "), 80), State: eff + " " + h.Kind, Hint: h.Origin, Search: h.Origin + " " + h.Kind})
	}
	l.Empty = "No knowledge yet. n adds a preference or decision; i previews a Classic import."
	l.set(rows)
	if cs, _ := u.rt.Knowledge.Conflicts(context.Background()); len(cs) > 0 {
		var keys []string
		for _, c := range cs {
			keys = append(keys, c.Key)
		}
		l.Err = "conflicting values for: " + strings.Join(keys, ", ")
	}
}

func (u *ultra) memoryDetail(id string) string {
	it, err := u.rt.Knowledge.Get(context.Background(), id)
	if err != nil {
		return err.Error()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s · %s · stored %s · effective %s\nOrigin: %s · scope %s\n", it.ID, it.Kind, it.Status, it.Effective(), it.Origin, it.Scope)
	if it.Verification != nil {
		fmt.Fprintf(&b, "Verified by %s (%s) %s: %s\n", it.Verification.By, it.Verification.Method, ustamp(it.Verification.Time), it.Verification.Evidence)
	}
	for _, s := range it.Sources {
		fmt.Fprintf(&b, "Source: %s %s %s\n", s.Kind, s.Ref, s.Note)
	}
	for _, d := range it.Deps {
		fmt.Fprintf(&b, "Depends on %s (%s)\n", d.Path, cellCut(d.Hash, 12))
	}
	if len(it.StaleFiles) > 0 {
		b.WriteString("STALE: changed " + strings.Join(it.StaleFiles, ", ") + "\n")
	}
	if it.Supersedes != "" {
		b.WriteString("Supersedes " + it.Supersedes + "\n")
	}
	if it.SupersededBy != "" {
		b.WriteString("Superseded by " + it.SupersededBy + "\n")
	}
	if it.Reason != "" {
		b.WriteString("Invalid: " + it.Reason + "\n")
	}
	if h, err := u.rt.Knowledge.History(context.Background(), id); err == nil {
		b.WriteString("History:\n  " + strings.Join(h, "\n  ") + "\n")
	}
	b.WriteString(strings.Repeat("─", 30) + "\n" + it.Text)
	return b.String()
}

// --- Runs -----------------------------------------------------------------

func (u *ultra) loadRuns(l *ulist) {
	ts, err := u.rt.Store.List()
	if err != nil {
		l.Err = err.Error()
	}
	var rows []urow
	for _, t := range ts {
		owner := ""
		if t.Owner != nil {
			owner = " · held by " + t.Owner.ID
		}
		mark := " "
		if t.ID == u.cur {
			mark = "▸"
		}
		state := string(t.State)
		if len(t.Unknown()) > 0 {
			state += ", unknown op"
		}
		rows = append(rows, urow{Key: t.ID, Mark: mark, Label: cellCut(t.Title, 50), State: state,
			Hint:   fmt.Sprintf("%d/%d · %s · %s · %d cp%s · %s", t.Attempts, t.MaxAttempts, t.Executor, project.RevisionLabel(t.ConfigRev), len(t.Checkpoints), owner, ustamp(t.Updated)),
			Search: t.ID + " " + t.Executor})
	}
	l.Empty = "No tasks yet. Describe one on the Workspace."
	l.set(rows)
}

func (u *ultra) runDetail(id string) string {
	t, err := u.rt.Store.Get(id)
	if err != nil {
		return err.Error()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s · %s · attempt %d of %d · executor %s\nConfig %s (latest %s) · mode %s\nObjective: %s\n", t.ID, strings.ToUpper(string(t.State)), t.Attempts, t.MaxAttempts,
		t.Executor, project.RevisionLabel(t.ConfigRev), project.RevisionLabel(u.proj.Revision()), t.Mode, t.Objective)
	if t.Owner != nil {
		fmt.Fprintf(&b, "Owner: %s (lease until %s, epoch %d)\n", t.Owner.ID, t.Owner.Until.Local().Format("15:04:05"), t.Owner.Epoch)
	}
	for _, c := range t.Criteria {
		fmt.Fprintf(&b, "Criterion %s: %s\n", c.ID, c.Describe())
	}
	if t.Validation != nil {
		for _, r := range t.Validation.Results {
			fmt.Fprintf(&b, "  [%s] %s %s\n", strings.ToUpper(r.Status), r.Criterion, cellCut(r.Detail, 100))
		}
	}
	if t.Report != nil {
		fmt.Fprintf(&b, "Executor claimed %s: %s\n", t.Report.Status, t.Report.Summary)
	}
	for _, x := range t.Constraints {
		b.WriteString("Constraint: " + x + "\n")
	}
	for _, x := range t.Decisions {
		b.WriteString("Decision: " + x + "\n")
	}
	for _, x := range t.Blockers {
		b.WriteString("Blocker: " + x + "\n")
	}
	for _, o := range t.Ops {
		fmt.Fprintf(&b, "Op %s %s %s %s %s\n", o.ID, o.Kind, strings.ToUpper(o.State), o.Ref, o.Note)
	}
	for _, x := range t.Links {
		b.WriteString("Linked: " + x + "\n")
	}
	if len(t.Checkpoints) > 0 {
		b.WriteString("Checkpoints: " + strings.Join(t.Checkpoints, ", ") + "\n")
	}
	if evs, err := u.rt.Store.Events(id); err == nil {
		b.WriteString("Events:\n")
		for _, e := range evs[max(0, len(evs)-30):] {
			fmt.Fprintf(&b, "  r%d %s %s %s %s\n", e.Rev, e.Time.Local().Format("15:04:05"), e.Type, e.Actor, e.Note)
		}
	}
	return b.String()
}

// --- Context --------------------------------------------------------------

func (u *ultra) loadContext(l *ulist) {
	t := u.curTask()
	if t == nil {
		draft := strings.TrimSpace(u.input.Text())
		if draft == "" {
			draft = "(no task: type an objective on the Workspace to preview its context)"
		}
		t = &task.Task{ID: "draft", Objective: draft, Workspace: u.proj.Root, Criteria: u.nextChecks}
	}
	cx, _, err := u.rt.Context(t, cmpStr(t.Executor, u.executor))
	if err != nil {
		l.Err = err.Error()
		l.set(nil)
		return
	}
	u.ctxNow = &cx
	var rows []urow
	for _, it := range cx.Items {
		mark := "-"
		switch {
		case it.Included:
			mark = "+"
		case it.Loaded == ctxsel.LoadedUnknown || it.Loaded == ctxsel.LoadedNative:
			mark = "n"
		}
		must := ""
		if it.Mandatory {
			must = "mandatory · "
		}
		rows = append(rows, urow{Key: it.ID, Mark: mark, Label: it.ID, State: fmt.Sprintf("%s · ~%d", it.Kind, it.Tokens), Hint: must + it.Loaded + " · " + it.Reason})
	}
	l.set(rows)
	if len(cx.Warnings) > 0 {
		l.Err = strings.Join(cx.Warnings, "; ")
	}
}

func (u *ultra) contextDetail(id string) string {
	if u.ctxNow == nil {
		return ""
	}
	for _, it := range u.ctxNow.Items {
		if it.ID == id {
			text := it.Text
			if text == "" {
				text = "(not in the prompt PAI builds)"
			}
			return fmt.Sprintf("%s · %s · source %s\nIncluded: %v · mandatory: %v · status: %s\nWhy: %s\n~%d tokens (estimate)\n%s\n%s",
				it.ID, it.Kind, it.Source, it.Included, it.Mandatory, it.Loaded, it.Reason, it.Tokens, strings.Repeat("─", 30), text)
		}
	}
	return id
}

// inspect fills the detail pane for the selected row.
func (u *ultra) inspect() {
	r, ok := u.lists[u.view].current()
	if !ok {
		u.detail.set("")
		return
	}
	var s string
	switch u.view {
	case vSkills:
		s = u.skillDetail(r.Key)
	case vInstr:
		s = u.instructionDetail(r.Key)
	case vMemory:
		s = u.memoryDetail(r.Key)
	case vRuns:
		s = u.runDetail(r.Key)
	case vContext:
		s = u.contextDetail(r.Key)
	}
	u.detail.set(s)
	u.detail.ink = nil
}

// --- Actions --------------------------------------------------------------

// action is a management view's own key. Every change to configuration is
// staged; Apply writes manifest and lock together, or nothing.
func (u *ultra) action(k term.Key) bool {
	a := u.a
	r, has := u.lists[u.view].current()
	key := k.String()
	switch key {
	case "p":
		if u.view == vSkills || u.view == vInstr {
			u.previewStage()
			return true
		}
	case "a":
		if u.view == vSkills || u.view == vInstr {
			u.applyStage()
			return true
		}
	case "d":
		if u.view == vSkills || u.view == vInstr {
			if u.staged() > 0 {
				u.stage = nil
				a.Say("staged changes discarded")
				u.reload()
			}
			return true
		}
	}
	switch u.view {
	case vSkills:
		return u.skillAction(key, r, has)
	case vInstr:
		return u.instrAction(key, r, has)
	case vMemory:
		return u.memoryAction(key, r, has)
	case vRuns:
		return u.runsAction(key, r, has)
	}
	return false
}

func (u *ultra) skillAction(key string, r urow, has bool) bool {
	a := u.a
	if key == "r" {
		u.profileMenu()
		return true
	}
	if !has {
		return false
	}
	switch key {
	case " ", "space":
		st := u.stageOrNew()
		if r.Mark == "[x]" {
			st.Disable(project.KindSkill, r.Key)
			a.Say("staged: disable " + r.Key + " (affects the next execution; a running context keeps what it loaded)")
		} else {
			st.Enable(project.KindSkill, r.Key, "")
			a.Say("staged: enable " + r.Key)
		}
	case "m":
		rs, _ := u.resolved()
		en, ok := rs.Find(project.KindSkill, r.Key)
		if !ok {
			a.Say("enable it first")
			return true
		}
		next := map[project.Activation]project.Activation{project.Auto: project.Always, project.Always: project.Manual, project.Manual: project.Auto}[en.ActivationOf()]
		u.stageOrNew().SetActivation(project.KindSkill, r.Key, next)
		a.Say("staged: " + r.Key + " activation " + string(next))
	case "s":
		ask(a, "Path scope for "+r.Key, "globs separated by spaces, e.g. pkg/** web/**; empty: whole project", func(a *App, text string) {
			u.stageOrNew().SetPaths(project.KindSkill, r.Key, strings.Fields(text))
			u.reload()
		})
	case "u":
		u.stageOrNew().AcceptUpdate(project.KindSkill, r.Key)
		a.Say("staged: take the library's content for " + r.Key + " (p previews)")
	case "P":
		rs, _ := u.resolved()
		en, ok := rs.Find(project.KindSkill, r.Key)
		if !ok || en.Pkg.Meta.VersionOf() == "" {
			a.Say("only an enabled skill that declares a version can be pinned")
			return true
		}
		u.stageOrNew().Pin(project.KindSkill, r.Key, en.Pkg.Meta.VersionOf())
		a.Say("staged: pin " + r.Key + " to " + en.Pkg.Meta.VersionOf())
	case "o":
		u.overrideAndEdit(project.KindSkill, r.Key)
		return true
	default:
		return false
	}
	u.reload()
	return true
}

func (u *ultra) profileMenu() {
	installed, _ := u.proj.Installed()
	m := &Menu{}
	for _, pk := range installed {
		if pk.Kind != project.KindProfile {
			continue
		}
		name := pk.Name
		on := slices.Contains(u.proj.Manifest.Profiles, name)
		label := name
		if on {
			label += " (applied)"
		}
		m.Items = append(m.Items, MenuItem{Label: label, Hint: pk.Meta.Description, Pick: func(a *App, _ *MenuItem) {
			st := u.stageOrNew()
			if on {
				st.RemoveProfile(name)
			} else {
				st.ApplyProfile(name)
			}
			a.Say("staged profile change; p previews, a applies")
			u.reload()
		}})
	}
	if len(m.Items) == 0 {
		u.a.Say("no profiles installed (~/.pai/ultra/library/profiles/<name>.yaml)")
		return
	}
	u.a.OpenMenu("Profiles", m)
}

func (u *ultra) instrAction(key string, r urow, has bool) bool {
	a := u.a
	switch key {
	case "c":
		tv := &textView{}
		tv.set(u.conflictsText())
		a.Dialog("Conflicts and duplicates", &Node{View: tv})
		return true
	case "n":
		ask(a, "New project rule", "name, e.g. testing", func(a *App, name string) {
			name = strings.TrimSpace(name)
			if name == "" || strings.ContainsAny(name, `/\.: `) {
				a.Say("a rule name is a single word")
				return
			}
			path := u.proj.Path("rules", name+".md")
			if _, err := os.Stat(path); err == nil {
				a.Say(name + " exists: select it and press e")
				return
			}
			u.edit("New rule "+name, fmt.Sprintf("---\ndescription: \nmandatory: true\n---\n# %s\n\n", name), func(text string) error {
				return u.stageOrNew().SetFile(path, []byte(text))
			})
		})
		return true
	}
	if !has {
		return false
	}
	sel := u.selection()
	i := slices.IndexFunc(sel.Applied, func(x project.Applied) bool { return x.ID == r.Key })
	if i < 0 {
		return false
	}
	in := sel.Applied[i]
	switch key {
	case "e":
		if !in.Editable() {
			a.Say(in.Owner + " instructions are read-only here: o makes a project " + map[bool]string{true: "copy", false: "override"}[in.Owner == project.OwnerGlobal])
			return true
		}
		raw, err := os.ReadFile(in.Path)
		if err != nil {
			a.SayError(err) // never open an empty editor over a file it could not read
			return true
		}
		u.edit("Edit "+in.Name, string(raw), func(text string) error { return u.stageOrNew().SetFile(in.Path, []byte(text)) })
	case "o":
		switch in.Owner {
		case project.OwnerPackage:
			u.overrideAndEdit(project.KindRule, in.Name)
		case project.OwnerGlobal:
			path := u.proj.Path("rules", strings.TrimSuffix(strings.Trim(strings.ReplaceAll(in.Name, "/", "-"), "~-."), ".md")+"-project.md")
			raw, err := os.ReadFile(in.Path)
			if err != nil {
				a.SayError(err)
				return true
			}
			u.edit("Project copy of "+in.Name, string(raw), func(text string) error { return u.stageOrNew().SetFile(path, []byte(text)) })
		default:
			a.Say("project instructions are edited in place: press e")
		}
	default:
		return false
	}
	return true
}

// overrideAndEdit stages a project override of a package's text and opens it.
func (u *ultra) overrideAndEdit(kind, name string) {
	st := u.stageOrNew()
	path := u.proj.OverridePath(kind, name)
	start, err := os.ReadFile(path)
	if err != nil {
		r, _ := u.resolved()
		en, ok := r.Find(kind, name)
		if !ok {
			u.a.Say("enable it first: only an enabled package can be overridden")
			return
		}
		start = en.Pkg.Raw
	}
	u.edit("Override "+name+" (the package is not changed)", string(start), func(text string) error {
		return st.SetFile(path, []byte(text))
	})
}

// edit opens an in-app editor on text; Ctrl+S stages it, Esc cancels.
func (u *ultra) edit(title, text string, save func(string) error) {
	in := NewInput()
	in.Cursor = true
	in.MaxRows = max(6, u.a.H-10)
	in.insert(textElems(text)...)
	var l *Layer
	in.Before = func(a *App, k term.Key) bool {
		switch k.String() {
		case "enter":
			in.insert(textElems("\n")...)
			return true
		case "ctrl+s":
			if err := save(in.Text()); err != nil {
				a.SayError(err)
				return true
			}
			a.Pop(l)
			u.refocus()
			a.Say("staged: p previews the diff, a applies, d discards")
			u.reload()
			return true
		}
		return false
	}
	l = u.a.Dialog(title+" · Ctrl+S stage · Esc cancel", &Node{View: in})
	l.Esc = u.refocus
	l.Place = func(w, h int) Rect { return Rect{2, 1, max(10, w-4), max(6, h-2)} }
}

func (u *ultra) previewStage() {
	if u.staged() == 0 {
		u.a.Say("nothing staged")
		return
	}
	pv, err := u.stage.Preview()
	if err != nil {
		u.a.SayError(err)
		return
	}
	var b strings.Builder
	for _, l := range u.stage.Log {
		b.WriteString("· " + l + "\n")
	}
	for _, p := range pv.Problems {
		b.WriteString(p.String() + "\n")
	}
	for _, d := range pv.Diffs {
		b.WriteString("--- " + d.Path + "\n" + d.Diff)
	}
	b.WriteString(fmt.Sprintf("\nApply makes %s; running tasks keep their revision.", project.RevisionLabel(pv.Revision)))
	tv := &textView{ink: diffInk}
	tv.set(b.String())
	u.a.Dialog("Staged changes", &Node{View: tv})
}

func (u *ultra) applyStage() {
	if u.staged() == 0 {
		u.a.Say("nothing staged")
		return
	}
	rev, err := u.stage.Apply()
	var inv *project.InvalidError
	switch {
	case errors.As(err, &inv):
		tv := &textView{}
		var lines []string
		for _, p := range inv.Problems {
			lines = append(lines, p.String())
		}
		tv.set("Nothing was written. Fix these, or d to discard:\n\n" + strings.Join(lines, "\n"))
		u.a.Dialog("Invalid configuration", &Node{View: tv})
		u.lists[u.view].Err = "invalid configuration: nothing written"
		return
	case errors.Is(err, project.ErrConflict):
		u.lists[u.view].Err = "conflict: the configuration changed elsewhere; d discards your staged edits so you can redo them"
		u.a.SayError(err)
		return
	case err != nil:
		u.a.SayError(err)
		return
	}
	u.stage = nil
	msg := "applied " + project.RevisionLabel(rev) + ": next executions use it"
	if u.running {
		msg += "; the running task keeps its revision until a restart"
	}
	u.a.Say(msg)
	u.reload()
}

func (u *ultra) memoryAction(key string, r urow, has bool) bool {
	a := u.a
	k := u.rt.Knowledge
	ctx := context.Background()
	switch key {
	case "s":
		u.memAll = !u.memAll
		u.reload()
		return true
	case "n":
		ask(a, "Add knowledge", "kind: text — kind is preference, decision, fact or hypothesis", func(a *App, text string) {
			kind, body, ok := strings.Cut(text, ":")
			if !ok {
				a.Say("write it as kind: text")
				return
			}
			it, err := k.Add(ctx, knowledge.Item{Kind: strings.TrimSpace(kind), Text: strings.TrimSpace(body), Origin: "person", Sources: []knowledge.Source{{Kind: "person", Ref: "tui"}}})
			if err != nil {
				a.SayError(err)
				return
			}
			a.Say(it.ID + " stored as " + it.Status)
			u.reload()
		})
		return true
	case "i":
		u.importPreview()
		return true
	}
	if !has {
		return false
	}
	switch key {
	case "v":
		if _, err := k.Verify(ctx, r.Key, knowledge.Verification{Method: "person", By: "person"}); err != nil {
			a.SayError(err)
		} else {
			a.Say(r.Key + " verified by you")
		}
	case "x":
		ask(a, "Invalidate "+r.Key, "why it no longer holds", func(a *App, why string) {
			if _, err := k.Invalidate(ctx, r.Key, "person", why); err != nil {
				a.SayError(err)
			}
			u.reload()
		})
		return true
	default:
		return false
	}
	u.reload()
	return true
}

// importPreview fetches Classic memory and shows what an import would add,
// as candidates. Enter imports; nothing changes in Classic.
func (u *ultra) importPreview() {
	l := u.lists[vMemory]
	if u.c == nil {
		l.Err = "offline: importing Classic memory needs the PAI server"
		return
	}
	l.Loading = true
	u.a.Go(func() {
		items, err := u.rt.ClassicMemory(context.Background(), 500)
		var plan knowledge.ImportPlan
		if err == nil {
			plan, err = u.rt.Knowledge.PreviewImport(context.Background(), items)
		}
		u.a.Post(func() {
			l.Loading = false
			if err != nil {
				l.Err = "Classic memory: " + err.Error()
				return
			}
			var b strings.Builder
			fmt.Fprintf(&b, "%d to import as unverified candidates, %d skipped. Enter imports · Esc cancels\n\n", len(plan.Add), len(plan.Skip))
			for _, it := range plan.Add {
				b.WriteString("+ " + it.Origin + ": " + cellCut(strings.Join(strings.Fields(it.Text), " "), 90) + "\n")
			}
			for _, s := range plan.Skip {
				b.WriteString("- classic:" + s.ID + ": " + s.Why + "\n")
			}
			tv := &textView{ink: diffInk}
			tv.set(b.String())
			var dl *Layer
			dl = u.a.Dialog("Import Classic memory?", &Node{View: confirm{tv, func() {
				u.a.Pop(dl)
				n, err := u.rt.Knowledge.Import(context.Background(), plan)
				if err != nil {
					u.a.SayError(err)
				} else {
					u.a.Say(fmt.Sprintf("imported %d candidates; Classic memory is unchanged", n))
				}
				u.reload()
			}}})
		})
	})
}

func (u *ultra) runsAction(key string, r urow, has bool) bool {
	a := u.a
	if key == "R" {
		ids, err := u.rt.Recover()
		if err != nil {
			a.SayError(err)
		} else {
			a.Say(fmt.Sprintf("%d task(s) recovered", len(ids)))
		}
		u.reload()
		return true
	}
	if !has {
		return false
	}
	switch key {
	case "o":
		u.cur = r.Key
		u.setView(vWork)
		u.say("current task: "+r.Key, InkMuted)
	case "r":
		if u.running {
			a.Say("a task is running")
			return true
		}
		u.cur = r.Key
		u.setView(vWork)
		u.say("resuming "+r.Key, InkMuted)
		u.run()
	case "c":
		if cp, err := u.rt.Store.Checkpoint(r.Key); err != nil {
			a.SayError(err)
		} else {
			a.Say("checkpoint " + cp.ID)
		}
		u.reload()
	case "h":
		u.cur = r.Key
		u.handoffMenu()
	case "A":
		t, err := u.rt.Store.Get(r.Key)
		if err != nil {
			a.SayError(err)
			return true
		}
		m := &Menu{}
		for _, cr := range t.Criteria {
			if cr.Kind != task.CriterionReview {
				continue
			}
			id := cr.ID
			m.Items = append(m.Items, MenuItem{Label: id, Hint: cr.Text, Pick: func(a *App, _ *MenuItem) {
				got, err := u.rt.Approve(context.Background(), r.Key, id, "person", "approved in the TUI")
				if err != nil {
					a.SayError(err)
				} else {
					a.Say(r.Key + " is " + string(got.State))
				}
				u.reload()
			}})
		}
		if len(m.Items) == 0 {
			a.Say("no review criteria")
			return true
		}
		a.OpenMenu("Approve a review (you, as a person)", m)
	case "u":
		t, err := u.rt.Store.Get(r.Key)
		if err != nil {
			a.SayError(err)
			return true
		}
		m := &Menu{}
		for _, o := range t.Unknown() {
			if o.State != task.OpUnknown {
				continue
			}
			op := o
			for _, st := range []string{task.OpDone, task.OpFailed} {
				m.Items = append(m.Items, MenuItem{Section: op.ID + " " + op.Desc, Label: "it " + map[string]string{task.OpDone: "completed", task.OpFailed: "did not complete"}[st], Pick: func(a *App, _ *MenuItem) {
					if _, err := u.rt.Store.ResolveOp(r.Key, op.ID, st, "reconciled in the TUI after inspecting the workspace"); err != nil {
						a.SayError(err)
					}
					u.reload()
				}})
			}
		}
		if len(m.Items) == 0 {
			a.Say("no unknown operations")
			return true
		}
		a.OpenMenu("Reconcile: inspect the workspace first", m)
	default:
		return false
	}
	return true
}

// ultraPending is a handoff waiting for Classic session id, from the
// project of the working directory; offer marks it offered.
func ultraPending(id string) (string, func(), bool) {
	dir, err := os.Getwd()
	if err != nil {
		return "", nil, false
	}
	p, err := projectOf(dir)
	if err != nil {
		return "", nil, false
	}
	return handoff.PendingFor(p, id)
}

// refocus puts focus back on the current view after a dialog closes.
func (u *ultra) refocus() {
	switch {
	case u.view == vWork:
		u.a.Focus(u.inputRow.Kids[0].Kids[1])
	case u.focus == fDetail && u.showDetail:
		u.a.Focus(u.detailNode)
	default:
		u.a.Focus(u.listNode)
	}
}
