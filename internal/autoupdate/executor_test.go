package autoupdate

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/junkerderprovinz/shiplog/internal/model"
)

type fakeLister struct{ sts []model.UpdateStatus }

func (f fakeLister) List() ([]model.UpdateStatus, error) { return f.sts, nil }

// fakeUpdater is a fake host: it is both the Updater and the live container
// view (Inspector) the executor looks at. An Update records the call and then
// leaves the container running the digests pull[name] lists (the first is the
// primary one) — by default the new image of stNamed — so the executor's
// before/after check sees a realistic host.
type fakeUpdater struct {
	calls  []string
	failOn string
	sup    bool

	live    map[string]model.Container // what runs right now, by name (seeded by newExec)
	pull    map[string][]string        // digests an Update of name leaves running; absent → stNamed's new image
	listErr error                      // every live List fails
	// listErrAfter makes the live List fail once an Update has run (an update that
	// cannot be verified afterwards).
	listErrAfter bool
	lists        int
}

func (f *fakeUpdater) Supported() bool { return f.sup }

func (f *fakeUpdater) Update(_ context.Context, name string) error {
	f.calls = append(f.calls, name)
	if name == f.failOn {
		return errors.New("boom")
	}
	c := f.live[name]
	if ds, ok := f.pull[name]; ok {
		c.Digest, c.Digests = ds[0], ds
	} else {
		c.Digest, c.Digests = newDigest(name), []string{newDigest(name)}
	}
	f.live[name] = c
	return nil
}

func (f *fakeUpdater) List(context.Context) ([]model.Container, error) {
	f.lists++
	if f.listErr != nil || (f.listErrAfter && len(f.calls) > 0) {
		return nil, errors.New("docker socket gone")
	}
	out := make([]model.Container, 0, len(f.live))
	for _, c := range f.live {
		out = append(out, c)
	}
	return out, nil
}

func oldDigest(name string) string { return "sha256:old-" + name }
func newDigest(name string) string { return "sha256:new-" + name }

// stNamed is a status with a stale-looking pending update: the container runs
// sha256:old-<name> and the registry's newest image is sha256:new-<name>.
func stNamed(name string, k model.Kind) model.UpdateStatus {
	return model.UpdateStatus{
		Container:    model.Container{Name: name, Digest: oldDigest(name), Digests: []string{oldDigest(name)}},
		Kind:         k,
		NewestTag:    "new",
		NewestDigest: newDigest(name),
	}
}

// newExec builds an Executor over the given statuses with upd as both updater
// and live host, which starts out running each status's container as stored.
func newExec(upd *fakeUpdater, sts ...model.UpdateStatus) *Executor {
	upd.live = map[string]model.Container{}
	for _, st := range sts {
		upd.live[st.Container.Name] = st.Container
	}
	return NewExecutor(fakeLister{sts: sts}, upd, upd)
}

type errLister struct{}

func (errLister) List() ([]model.UpdateStatus, error) { return nil, errors.New("db down") }

func TestExecutorListErrorEmptyResult(t *testing.T) {
	upd := &fakeUpdater{sup: true}
	e := NewExecutor(errLister{}, upd, upd)
	res := e.Run(context.Background(), Policy{Level: LevelMajor}, false)
	if len(upd.calls) != 0 || len(res.Outcomes) != 0 {
		t.Fatalf("a lister error must yield an empty run with no updates, got calls=%v outcomes=%+v", upd.calls, res.Outcomes)
	}
}

func TestExecutorSerialEligibleOnly(t *testing.T) {
	upd := &fakeUpdater{sup: true}
	e := newExec(upd, stNamed("a", model.KindPatch), stNamed("b", model.KindMajor), stNamed("c", model.KindUnknown))
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
	e := newExec(upd, stNamed("a", model.KindPatch))
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
	e := newExec(upd, stNamed("a", model.KindPatch), stNamed("d", model.KindPatch))
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
	e := newExec(upd, stNamed("a", model.KindPatch))
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
	e := newExec(upd,
		stWithChangelog("a", model.KindPatch, "This release includes a BREAKING change."),
		stWithChangelog("b", model.KindPatch, "Just bug fixes."),
	)
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
	e := newExec(upd, stWithChangelog("a", model.KindPatch, "BREAKING: config format changed"))
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
	e := newExec(upd, stWithChangelog("a", model.KindPatch, "BREAKING: this text is irrelevant with no configured words"))
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
	e := newExec(upd, stNamed("Cody", model.KindMinor), stNamed("plex", model.KindMinor))
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
		e := newExec(upd, stNamed("Cody", model.KindPatch))
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
	e := newExec(upd, stNamed("Cody", model.KindMinor), stNamed("plex", model.KindMinor))
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
	e := newExec(upd, stNamed("a", model.KindPatch), stNamed("b", model.KindPatch))
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
		e := newExec(upd, stNamed("Cody", model.KindMinor), stNamed("plex", model.KindMajor), stNamed("old", model.KindUnknown))
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
	e := newExec(upd, stNamed("Cody", model.KindNone), stNamed("plex", model.KindMajor))
	res := e.Run(context.Background(), Policy{Level: LevelMinor, ExcludeContainers: []string{"Cody", "plex"}}, false)
	if len(upd.calls) != 0 || len(res.Outcomes) != 0 {
		t.Fatalf("calls=%v outcomes=%+v, want neither", upd.calls, res.Outcomes)
	}
}

func TestExecutorExcludedContainerIsSkippedNotBlocked(t *testing.T) {
	// When both guards apply, the admin's explicit container choice is the
	// reported reason — not the changelog word.
	upd := &fakeUpdater{sup: true}
	e := newExec(upd, stWithChangelog("Cody", model.KindPatch, "BREAKING: config format changed"))
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

// --- live before/after checks -------------------------------------------------

// The stored verdict can be stale: the container already runs the newest image of
// its tag (sonarr and friends were "major" with digest == newest digest). That is
// nothing to do — never an Updater call, never a success — in real and dry runs.
func TestExecutorSkipsContainerAlreadyOnNewestImage(t *testing.T) {
	for _, dry := range []bool{false, true} {
		sonarr := stNamed("sonarr", model.KindMajor)
		sonarr.NewestDigest = sonarr.Container.Digest // registry == running
		upd := &fakeUpdater{sup: true}
		e := newExec(upd, sonarr, stNamed("plex", model.KindDigest))
		res := e.Run(context.Background(), Policy{Level: LevelMajor, Digest: true}, dry)

		wantCalls := []string{"plex"} // only the container that really has something to pull
		if dry {
			wantCalls = nil // a dry run never calls Update
		}
		if !reflect.DeepEqual(upd.calls, wantCalls) {
			t.Fatalf("dry=%v: Update calls = %v, want %v — the up-to-date container must not be touched", dry, upd.calls, wantCalls)
		}
		if len(res.Outcomes) != 2 {
			t.Fatalf("dry=%v: outcomes = %+v", dry, res.Outcomes)
		}
		s := res.Outcomes[0]
		if s.Name != "sonarr" || !s.UpToDate || s.Updated || s.Err != nil || s.Skipped || s.Blocked {
			t.Fatalf("dry=%v: sonarr = %+v, want UpToDate only (not Updated, no Err)", dry, s)
		}
		p := res.Outcomes[1]
		if p.UpToDate || !p.Updated || p.Err != nil {
			t.Fatalf("dry=%v: plex = %+v, want a plain Updated / would-update", dry, p)
		}
	}
}

// A mirror pull carries one RepoDigests entry per registry: matching ANY of them
// means the container already runs the newest image.
func TestExecutorUpToDateThroughMirrorDigest(t *testing.T) {
	st := stNamed("mirrored", model.KindDigest)
	st.Container.Digest = "sha256:mirror"
	st.Container.Digests = []string{"sha256:mirror", "sha256:upstream"}
	st.NewestDigest = "sha256:upstream"
	upd := &fakeUpdater{sup: true}
	res := newExec(upd, st).Run(context.Background(), Policy{Level: LevelMajor, Digest: true}, false)
	if len(upd.calls) != 0 || len(res.Outcomes) != 1 || !res.Outcomes[0].UpToDate {
		t.Fatalf("calls=%v outcomes=%+v, want one UpToDate and no update", upd.calls, res.Outcomes)
	}
}

// The check looks at the container as it runs NOW, not at the stored row: the
// row says "update" but the user already updated it by hand since the last scan.
func TestExecutorUsesLiveStateNotTheStoredRow(t *testing.T) {
	st := stNamed("radarr", model.KindMinor) // stored: still on the old image
	upd := &fakeUpdater{sup: true}
	e := newExec(upd, st)
	live := upd.live["radarr"]
	live.Digest, live.Digests = st.NewestDigest, []string{st.NewestDigest} // updated by hand meanwhile
	upd.live["radarr"] = live
	res := e.Run(context.Background(), Policy{Level: LevelMinor}, false)
	if len(upd.calls) != 0 || len(res.Outcomes) != 1 || !res.Outcomes[0].UpToDate {
		t.Fatalf("calls=%v outcomes=%+v, want nothing to do", upd.calls, res.Outcomes)
	}
}

// A real update: the container ends up on the expected, different image.
func TestExecutorVerifiesTheImageChanged(t *testing.T) {
	upd := &fakeUpdater{sup: true}
	e := newExec(upd, stNamed("SearXNG", model.KindDigest))
	res := e.Run(context.Background(), Policy{Digest: true}, false)
	o := res.Outcomes[0]
	if !o.Updated || o.Err != nil || o.UpToDate {
		t.Fatalf("outcome = %+v, want a verified Updated", o)
	}
	if got := upd.live["SearXNG"].Digest; got != newDigest("SearXNG") {
		t.Fatalf("live digest = %s, the fake host did not move", got)
	}
	if upd.lists < 2 {
		t.Fatalf("the host must be inspected before and after the update, lists=%d", upd.lists)
	}
}

// The Unraid script exits 0 but the container still runs the same image (an
// unchanged tag re-pulled, or a recreate that silently fell back). That is a
// failure, never a success — the bug that logged every daily no-op restart as ok.
func TestExecutorNoChangeIsAFailure(t *testing.T) {
	st := stNamed("wyoming", model.KindMinor)
	upd := &fakeUpdater{sup: true, pull: map[string][]string{"wyoming": {oldDigest("wyoming")}}} // the pull yields the same image
	res := newExec(upd, st).Run(context.Background(), Policy{Level: LevelMajor}, false)
	o := res.Outcomes[0]
	if o.Updated {
		t.Fatalf("a no-change update must not be reported as Updated: %+v", o)
	}
	if !errors.Is(o.Err, ErrNoChange) || o.Err.Error() != "update did not change the image" {
		t.Fatalf("err = %v, want ErrNoChange (update did not change the image)", o.Err)
	}
	// The failure text reaches the run summary.
	if text, _ := RenderSummary(res); !strings.Contains(text, "1 failed") || !strings.Contains(text, "update did not change the image") {
		t.Fatalf("summary should report the failure: %s", text)
	}
}

// A re-pull that reorders a mirror-pulled image's RepoDigests (so the primary
// digest differs) is still the same image: any shared digest means no change.
func TestExecutorNoChangeDetectedAcrossMirrorDigests(t *testing.T) {
	st := stNamed("mirrored", model.KindDigest)
	st.Container.Digest = "sha256:mirror"
	st.Container.Digests = []string{"sha256:mirror", "sha256:upstream-old"}
	upd := &fakeUpdater{sup: true, pull: map[string][]string{"mirrored": {"sha256:upstream-old", "sha256:mirror"}}}
	res := newExec(upd, st).Run(context.Background(), Policy{Digest: true}, false)
	if o := res.Outcomes[0]; o.Updated || !errors.Is(o.Err, ErrNoChange) {
		t.Fatalf("outcome = %+v, want ErrNoChange", o)
	}
}

// The container did change, but not to the image ShipLog expected (the registry
// moved again between the scan and the update): not a verified success.
func TestExecutorChangedButUnexpectedImageIsAFailure(t *testing.T) {
	st := stNamed("qbittorrent", model.KindDigest)
	upd := &fakeUpdater{sup: true, pull: map[string][]string{"qbittorrent": {"sha256:somethingelse0000"}}}
	res := newExec(upd, st).Run(context.Background(), Policy{Digest: true}, false)
	o := res.Outcomes[0]
	if o.Updated || o.Err == nil || errors.Is(o.Err, ErrNoChange) {
		t.Fatalf("outcome = %+v, want a failure that is NOT 'no change'", o)
	}
	for _, want := range []string{"somethingels", "new-qbittorr", "expected"} { // digests are shown cut to 12 characters
		if !strings.Contains(o.Err.Error(), want) {
			t.Errorf("error %q should name both images (missing %q)", o.Err, want)
		}
	}
}

// Without an expected digest (an old row) the check can only demand a change.
func TestExecutorWithoutExpectedDigestOnlyDemandsAChange(t *testing.T) {
	st := stNamed("legacy", model.KindDigest)
	st.NewestDigest = ""
	upd := &fakeUpdater{sup: true}
	res := newExec(upd, st).Run(context.Background(), Policy{Digest: true}, false)
	if o := res.Outcomes[0]; !o.Updated || o.Err != nil {
		t.Fatalf("outcome = %+v, want Updated (the image did change)", o)
	}
	upd2 := &fakeUpdater{sup: true, pull: map[string][]string{"legacy": {oldDigest("legacy")}}}
	res = newExec(upd2, st).Run(context.Background(), Policy{Digest: true}, false)
	if o := res.Outcomes[0]; o.Updated || !errors.Is(o.Err, ErrNoChange) {
		t.Fatalf("outcome = %+v, want ErrNoChange even without an expected digest", o)
	}
}

// If the host cannot be looked at before the update there is no baseline to
// verify against: nothing is updated and the failure is reported.
func TestExecutorNeverUpdatesWhatItCannotInspect(t *testing.T) {
	upd := &fakeUpdater{sup: true, listErr: errors.New("x")}
	res := newExec(upd, stNamed("a", model.KindMinor)).Run(context.Background(), Policy{Level: LevelMinor}, false)
	if len(upd.calls) != 0 {
		t.Fatalf("Update called for %v although the container could not be inspected", upd.calls)
	}
	if o := res.Outcomes[0]; o.Updated || o.Err == nil || !strings.Contains(o.Err.Error(), "inspect") {
		t.Fatalf("outcome = %+v, want an inspection failure", o)
	}
}

func TestExecutorContainerGoneBeforeUpdate(t *testing.T) {
	upd := &fakeUpdater{sup: true}
	e := newExec(upd, stNamed("ghost", model.KindMinor))
	delete(upd.live, "ghost") // removed since the last scan
	res := e.Run(context.Background(), Policy{Level: LevelMinor}, false)
	if len(upd.calls) != 0 || res.Outcomes[0].Err == nil || res.Outcomes[0].Updated {
		t.Fatalf("calls=%v outcome=%+v, want a not-found failure and no update", upd.calls, res.Outcomes[0])
	}
}

// The update ran, but the host cannot be inspected afterwards: unverifiable is
// not a success.
func TestExecutorUnverifiableUpdateIsAFailure(t *testing.T) {
	upd := &fakeUpdater{sup: true, listErrAfter: true}
	res := newExec(upd, stNamed("a", model.KindMinor)).Run(context.Background(), Policy{Level: LevelMinor}, false)
	if len(upd.calls) != 1 {
		t.Fatalf("the update itself should have run once, calls=%v", upd.calls)
	}
	if o := res.Outcomes[0]; o.Updated || o.Err == nil || !strings.Contains(o.Err.Error(), "could not be verified") {
		t.Fatalf("outcome = %+v, want a 'could not be verified' failure", o)
	}
}

// A pinned-tag advisory (a newer version tag exists, but re-pulling the pinned
// tag changes nothing) is never auto-updated — at the most permissive policy.
func TestExecutorNeverUpdatesAPinnedAdvisory(t *testing.T) {
	wyoming := model.UpdateStatus{
		Container:      model.Container{Name: "wyoming-openai", Tag: "0.6.1", Digest: "sha256:7200", Digests: []string{"sha256:7200"}},
		RunningVersion: "0.6.1", NewestTag: "0.7.0", NewestDigest: "sha256:7200",
		Kind: model.KindNone, Risk: model.RiskNone, NewerVersion: "0.7.0",
	}
	for _, dry := range []bool{false, true} {
		upd := &fakeUpdater{sup: true}
		res := newExec(upd, wyoming).Run(context.Background(), Policy{Level: LevelMajor, Digest: true}, dry)
		if len(upd.calls) != 0 || len(res.Outcomes) != 0 {
			t.Fatalf("dry=%v: calls=%v outcomes=%+v, an advisory must produce no update and no outcome", dry, upd.calls, res.Outcomes)
		}
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
		{"only up to date", Result{Outcomes: []Outcome{{Name: "sonarr", UpToDate: true}}}, false},
		{"skipped + up to date", Result{Outcomes: []Outcome{{Name: "Cody", Skipped: true}, {Name: "sonarr", UpToDate: true}}}, false},
		{"up to date + updated", Result{Outcomes: []Outcome{{Name: "sonarr", UpToDate: true}, {Name: "plex", Updated: true}}}, true},
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
