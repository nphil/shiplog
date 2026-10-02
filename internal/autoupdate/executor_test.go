package autoupdate

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/junkerderprovinz/shiplog/internal/model"
)

type fakeLister struct{ sts []model.UpdateStatus }

func (f fakeLister) List() ([]model.UpdateStatus, error) { return f.sts, nil }

type fakeUpdater struct {
	calls  []string
	failOn string
	sup    bool
}

func (f *fakeUpdater) Supported() bool { return f.sup }
func (f *fakeUpdater) Update(_ context.Context, name string) error {
	f.calls = append(f.calls, name)
	if name == f.failOn {
		return errors.New("boom")
	}
	return nil
}

func stNamed(name string, k model.Kind) model.UpdateStatus {
	return model.UpdateStatus{Container: model.Container{Name: name}, Kind: k, NewestTag: "new"}
}

type errLister struct{}

func (errLister) List() ([]model.UpdateStatus, error) { return nil, errors.New("db down") }

func TestExecutorListErrorEmptyResult(t *testing.T) {
	upd := &fakeUpdater{sup: true}
	e := NewExecutor(errLister{}, upd)
	res := e.Run(context.Background(), Policy{Level: LevelMajor}, false)
	if len(upd.calls) != 0 || len(res.Outcomes) != 0 {
		t.Fatalf("a lister error must yield an empty run with no updates, got calls=%v outcomes=%+v", upd.calls, res.Outcomes)
	}
}

func TestExecutorSerialEligibleOnly(t *testing.T) {
	upd := &fakeUpdater{sup: true}
	e := NewExecutor(fakeLister{sts: []model.UpdateStatus{
		stNamed("a", model.KindPatch), stNamed("b", model.KindMajor), stNamed("c", model.KindUnknown),
	}}, upd)
	res := e.Run(context.Background(), Policy{Level: LevelPatch}, false)
	if !reflect.DeepEqual(upd.calls, []string{"a"}) {
		t.Fatalf("updated %v, want [a] (only patch under patch)", upd.calls)
	}
	if len(res.Outcomes) != 1 || !res.Outcomes[0].Updated {
		t.Fatalf("outcomes %+v", res.Outcomes)
	}
}

func TestExecutorDryRunAppliesNothing(t *testing.T) {
	upd := &fakeUpdater{sup: true}
	e := NewExecutor(fakeLister{sts: []model.UpdateStatus{stNamed("a", model.KindPatch)}}, upd)
	res := e.Run(context.Background(), Policy{Level: LevelMajor}, true)
	if len(upd.calls) != 0 {
		t.Fatal("dry-run must not call Update")
	}
	if !res.DryRun || len(res.Outcomes) != 1 || !res.Outcomes[0].Updated {
		t.Fatalf("dry-run outcome should mark would-update: %+v", res)
	}
}

func TestExecutorFailureDoesNotAbort(t *testing.T) {
	upd := &fakeUpdater{sup: true, failOn: "a"}
	e := NewExecutor(fakeLister{sts: []model.UpdateStatus{stNamed("a", model.KindPatch), stNamed("d", model.KindPatch)}}, upd)
	res := e.Run(context.Background(), Policy{Level: LevelPatch}, false)
	if len(upd.calls) != 2 || upd.calls[1] != "d" {
		t.Fatalf("must continue past a failure to d, calls=%v", upd.calls)
	}
	if res.Outcomes[0].Err == nil || res.Outcomes[1].Err != nil {
		t.Fatalf("a should fail, d succeed: %+v", res.Outcomes)
	}
}

func TestExecutorUnsupportedNoop(t *testing.T) {
	upd := &fakeUpdater{sup: false}
	e := NewExecutor(fakeLister{sts: []model.UpdateStatus{stNamed("a", model.KindPatch)}}, upd)
	res := e.Run(context.Background(), Policy{Level: LevelMajor}, false)
	if len(upd.calls) != 0 || len(res.Outcomes) != 0 {
		t.Fatal("an unsupported updater must be a no-op")
	}
}

func stWithChangelog(name string, k model.Kind, raw string) model.UpdateStatus {
	s := stNamed(name, k)
	s.Changelog = &model.Changelog{Raw: raw}
	return s
}

func TestExecutorExcludeWordBlocksRealUpdate(t *testing.T) {
	upd := &fakeUpdater{sup: true}
	e := NewExecutor(fakeLister{sts: []model.UpdateStatus{
		stWithChangelog("a", model.KindPatch, "This release includes a BREAKING change."),
		stWithChangelog("b", model.KindPatch, "Just bug fixes."),
	}}, upd)
	res := e.Run(context.Background(), Policy{Level: LevelPatch, ExcludeWords: []string{"breaking"}}, false)
	if !reflect.DeepEqual(upd.calls, []string{"b"}) {
		t.Fatalf("Updater.Update calls = %v, want only [b] — the blocked container must never be applied", upd.calls)
	}
	if len(res.Outcomes) != 2 {
		t.Fatalf("want 2 outcomes (one blocked, one updated), got %+v", res.Outcomes)
	}
	a, b := res.Outcomes[0], res.Outcomes[1]
	if !a.Blocked || a.BlockedWord != "breaking" || a.Updated || a.Err != nil {
		t.Fatalf("outcome a = %+v, want Blocked with word 'breaking', not Updated, no Err", a)
	}
	if b.Blocked || !b.Updated || b.Err != nil {
		t.Fatalf("outcome b = %+v, want plain Updated", b)
	}
}

func TestExecutorExcludeWordBlocksDryRunToo(t *testing.T) {
	// The safety switch must be visible in dry-run — an admin verifying it works
	// before arming real updates must see "would be blocked", not "would update".
	upd := &fakeUpdater{sup: true}
	e := NewExecutor(fakeLister{sts: []model.UpdateStatus{
		stWithChangelog("a", model.KindPatch, "BREAKING: config format changed"),
	}}, upd)
	res := e.Run(context.Background(), Policy{Level: LevelPatch, ExcludeWords: []string{"breaking"}}, true)
	if len(upd.calls) != 0 {
		t.Fatal("dry-run must never call Update, blocked or not")
	}
	if len(res.Outcomes) != 1 || !res.Outcomes[0].Blocked || res.Outcomes[0].Updated {
		t.Fatalf("dry-run blocked outcome = %+v, want Blocked=true Updated=false", res.Outcomes)
	}
}

func TestExecutorNoExcludeWordsConfiguredUpdatesNormally(t *testing.T) {
	upd := &fakeUpdater{sup: true}
	e := NewExecutor(fakeLister{sts: []model.UpdateStatus{
		stWithChangelog("a", model.KindPatch, "BREAKING: this text is irrelevant with no configured words"),
	}}, upd)
	res := e.Run(context.Background(), Policy{Level: LevelPatch}, false) // ExcludeWords nil
	if !reflect.DeepEqual(upd.calls, []string{"a"}) {
		t.Fatalf("with no exclude words configured the update must proceed normally, calls=%v", upd.calls)
	}
	if res.Outcomes[0].Blocked {
		t.Fatal("must not be marked Blocked when no exclude words are configured")
	}
}

func TestExecutorExcludedContainerNeverUpdated(t *testing.T) {
	upd := &fakeUpdater{sup: true}
	e := NewExecutor(fakeLister{sts: []model.UpdateStatus{
		stNamed("Cody", model.KindMinor),
		stNamed("plex", model.KindMinor),
	}}, upd)
	res := e.Run(context.Background(), Policy{Level: LevelMajor, ExcludeContainers: []string{"Cody"}}, false)
	if !reflect.DeepEqual(upd.calls, []string{"plex"}) {
		t.Fatalf("Updater.Update calls = %v, want only [plex] — an excluded container must never be applied", upd.calls)
	}
	if len(res.Outcomes) != 2 {
		t.Fatalf("want 2 outcomes (one skipped, one updated), got %+v", res.Outcomes)
	}
	cody, plex := res.Outcomes[0], res.Outcomes[1]
	if !cody.Skipped || cody.Updated || cody.Blocked || cody.Err != nil {
		t.Fatalf("Cody = %+v, want Skipped only (not Updated, not Blocked, no Err)", cody)
	}
	if plex.Skipped || !plex.Updated || plex.Err != nil {
		t.Fatalf("plex = %+v, want a plain Updated outcome", plex)
	}
}

func TestExecutorExcludedMatchIsCaseInsensitive(t *testing.T) {
	for _, configured := range []string{"cody", "CODY", "Cody"} {
		upd := &fakeUpdater{sup: true}
		e := NewExecutor(fakeLister{sts: []model.UpdateStatus{stNamed("Cody", model.KindPatch)}}, upd)
		res := e.Run(context.Background(), Policy{Level: LevelPatch, ExcludeContainers: []string{configured}}, false)
		if len(upd.calls) != 0 {
			t.Errorf("configured %q: Update called for %v, want none", configured, upd.calls)
		}
		if len(res.Outcomes) != 1 || !res.Outcomes[0].Skipped {
			t.Errorf("configured %q: outcomes = %+v, want one Skipped", configured, res.Outcomes)
		}
	}
}

func TestExecutorExcludedContainerSkippedInDryRunToo(t *testing.T) {
	// An admin checking a dry run must see the exclusion working ("skipped"),
	// not "would update", before they trust it live.
	upd := &fakeUpdater{sup: true}
	e := NewExecutor(fakeLister{sts: []model.UpdateStatus{
		stNamed("Cody", model.KindMinor), stNamed("plex", model.KindMinor),
	}}, upd)
	res := e.Run(context.Background(), Policy{Level: LevelMinor, ExcludeContainers: []string{"Cody"}}, true)
	if len(upd.calls) != 0 {
		t.Fatalf("dry-run must never call Update, got %v", upd.calls)
	}
	if !res.DryRun || len(res.Outcomes) != 2 {
		t.Fatalf("dry-run result = %+v", res)
	}
	if !res.Outcomes[0].Skipped || res.Outcomes[0].Updated {
		t.Fatalf("Cody dry-run outcome = %+v, want Skipped and not 'would update'", res.Outcomes[0])
	}
	if res.Outcomes[1].Skipped || !res.Outcomes[1].Updated {
		t.Fatalf("plex dry-run outcome = %+v, want 'would update'", res.Outcomes[1])
	}
}

func TestExecutorNonExcludedContainersUnaffected(t *testing.T) {
	// A list that names only other containers changes nothing for everyone else.
	upd := &fakeUpdater{sup: true}
	e := NewExecutor(fakeLister{sts: []model.UpdateStatus{
		stNamed("a", model.KindPatch), stNamed("b", model.KindPatch),
	}}, upd)
	res := e.Run(context.Background(), Policy{Level: LevelPatch, ExcludeContainers: []string{"Cody", "plex"}}, false)
	if !reflect.DeepEqual(upd.calls, []string{"a", "b"}) {
		t.Fatalf("updated %v, want [a b]", upd.calls)
	}
	for _, o := range res.Outcomes {
		if o.Skipped || !o.Updated {
			t.Fatalf("outcome %+v, want a plain Updated", o)
		}
	}
}

func TestExecutorEmptyExcludeContainersChangesNothing(t *testing.T) {
	for _, list := range [][]string{nil, {}} {
		upd := &fakeUpdater{sup: true}
		e := NewExecutor(fakeLister{sts: []model.UpdateStatus{
			stNamed("Cody", model.KindMinor), stNamed("plex", model.KindMajor), stNamed("old", model.KindUnknown),
		}}, upd)
		res := e.Run(context.Background(), Policy{Level: LevelMajor, ExcludeContainers: list}, false)
		if !reflect.DeepEqual(upd.calls, []string{"Cody", "plex"}) {
			t.Fatalf("list=%v: updated %v, want [Cody plex] (the pre-existing behaviour)", list, upd.calls)
		}
		if len(res.Outcomes) != 2 || res.Outcomes[0].Skipped || res.Outcomes[1].Skipped {
			t.Fatalf("list=%v: outcomes %+v, want 2 updated, none skipped", list, res.Outcomes)
		}
	}
}

func TestExecutorExcludedContainerWithoutEligibleUpdateReportsNothing(t *testing.T) {
	// "Skipped" means "an update was due and we held it back". An excluded
	// container that is up to date, or whose bump is above the level, is simply
	// not eligible — no outcome, so no noise in the run log.
	upd := &fakeUpdater{sup: true}
	e := NewExecutor(fakeLister{sts: []model.UpdateStatus{
		stNamed("Cody", model.KindNone), stNamed("plex", model.KindMajor),
	}}, upd)
	res := e.Run(context.Background(), Policy{Level: LevelMinor, ExcludeContainers: []string{"Cody", "plex"}}, false)
	if len(upd.calls) != 0 || len(res.Outcomes) != 0 {
		t.Fatalf("calls=%v outcomes=%+v, want neither", upd.calls, res.Outcomes)
	}
}

func TestExecutorExcludedContainerIsSkippedNotBlocked(t *testing.T) {
	// When both guards apply, the admin's explicit container choice is the
	// reported reason — not the changelog word.
	upd := &fakeUpdater{sup: true}
	e := NewExecutor(fakeLister{sts: []model.UpdateStatus{
		stWithChangelog("Cody", model.KindPatch, "BREAKING: config format changed"),
	}}, upd)
	res := e.Run(context.Background(), Policy{
		Level: LevelPatch, ExcludeWords: []string{"breaking"}, ExcludeContainers: []string{"Cody"},
	}, false)
	if len(upd.calls) != 0 {
		t.Fatalf("Update called for %v, want none", upd.calls)
	}
	if len(res.Outcomes) != 1 || !res.Outcomes[0].Skipped || res.Outcomes[0].Blocked {
		t.Fatalf("outcomes = %+v, want Skipped and not Blocked", res.Outcomes)
	}
}

func TestResultNotable(t *testing.T) {
	cases := []struct {
		name string
		res  Result
		want bool
	}{
		{"empty run", Result{}, false},
		{"only skipped", Result{Outcomes: []Outcome{{Name: "Cody", Skipped: true}}}, false},
		{"skipped + updated", Result{Outcomes: []Outcome{{Name: "Cody", Skipped: true}, {Name: "plex", Updated: true}}}, true},
		{"skipped + blocked", Result{Outcomes: []Outcome{{Name: "Cody", Skipped: true}, {Name: "a", Blocked: true}}}, true},
		{"skipped + failed", Result{Outcomes: []Outcome{{Name: "Cody", Skipped: true}, {Name: "a", Err: errors.New("boom")}}}, true},
		{"only updated", Result{Outcomes: []Outcome{{Name: "plex", Updated: true}}}, true},
	}
	for _, c := range cases {
		if got := c.res.Notable(); got != c.want {
			t.Errorf("%s: Notable = %v, want %v", c.name, got, c.want)
		}
	}
}
