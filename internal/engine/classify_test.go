package engine

import (
	"context"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/junkerderprovinz/shiplog/internal/autoupdate"
	"github.com/junkerderprovinz/shiplog/internal/model"
)

// The cases below are the real containers behind the daily phantom restarts and
// the real moves that must keep working (digests shortened for readability).
//
//	sonarr   :latest running 4.0.20.3014-ls326 — registry's highest version tag is "5.14"
//	tautulli :latest running v2.18.2-ls246     — highest "version" tag is the date tag 2021.12.16
//	nzbget   :latest running v26.3-ls265      — highest "version" tag is the date tag 2021.11.25
//	CoralHub :latest running 0.0.0-dev        — highest version tag is 1.2.1
//
// For all four the container already runs the newest image of its own tag, so
// there is nothing to pull. wyoming-openai is pinned to :0.6.1 while 0.7.0
// exists; re-pulling :0.6.1 changes nothing.
func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		c    model.Container
		// running is what decideRunningVersion believed; the rest is what the
		// resolver returned for the container's own tag and for the registry's
		// numerically highest version tag.
		running, newestTag, newestDigest, verTag, verDigest string

		wantKind  model.Kind
		wantRisk  model.RiskLevel
		wantNewer string // pinned-tag advisory
		wantTo    string // end of the changelog span ("" = the newest tag)
	}{
		// --- phantom updates: the own tag's digest is unchanged → up to date ---
		{
			name: "sonarr: latest unchanged, junk higher version tag 5.14",
			c:    model.Container{Tag: "latest", Digest: "sha256:f247"}, running: "4.0.20.3014-ls326",
			newestTag: "latest", newestDigest: "sha256:f247", verTag: "5.14", verDigest: "sha256:v5dev",
			wantKind: model.KindNone, wantRisk: model.RiskNone, wantTo: "4.0.20.3014-ls326",
		},
		{
			name: "tautulli: latest unchanged, junk date tag 2021.12.16",
			c:    model.Container{Tag: "latest", Digest: "sha256:bfcd"}, running: "v2.18.2-ls246",
			newestTag: "latest", newestDigest: "sha256:bfcd", verTag: "2021.12.16", verDigest: "sha256:old2021",
			wantKind: model.KindNone, wantRisk: model.RiskNone, wantTo: "v2.18.2-ls246",
		},
		{
			name: "nzbget: latest unchanged, junk date tag 2021.11.25",
			c:    model.Container{Tag: "latest", Digest: "sha256:ac88"}, running: "v26.3-ls265",
			newestTag: "latest", newestDigest: "sha256:ac88", verTag: "2021.11.25", verDigest: "sha256:old2021b",
			wantKind: model.KindNone, wantRisk: model.RiskNone, wantTo: "v26.3-ls265",
		},
		{
			name: "CoralHub: latest unchanged, higher version tag 1.2.1",
			c:    model.Container{Tag: "latest", Digest: "sha256:9638"}, running: "0.0.0-dev",
			newestTag: "latest", newestDigest: "sha256:9638", verTag: "1.2.1", verDigest: "sha256:other",
			wantKind: model.KindNone, wantRisk: model.RiskNone, wantTo: "0.0.0-dev",
		},
		{
			name: "latest unchanged while a newer version tag exists with another digest",
			c:    model.Container{Tag: "latest", Digest: "sha256:cur"}, running: "1.8.0",
			newestTag: "latest", newestDigest: "sha256:cur", verTag: "1.9.0", verDigest: "sha256:next",
			wantKind: model.KindNone, wantRisk: model.RiskNone, wantTo: "1.8.0",
		},
		{
			name: "latest unchanged and equal to the newest version tag",
			c:    model.Container{Tag: "latest", Digest: "sha256:cur"}, running: "1.8.0",
			newestTag: "latest", newestDigest: "sha256:cur", verTag: "1.8.0", verDigest: "sha256:cur",
			wantKind: model.KindNone, wantRisk: model.RiskNone, wantTo: "1.8.0",
		},
		{
			name:    "mirror pull: the registry digest matches a secondary RepoDigest",
			c:       model.Container{Tag: "latest", Digest: "sha256:mirror", Digests: []string{"sha256:mirror", "sha256:up"}},
			running: "3.0.0", newestTag: "latest", newestDigest: "sha256:up", verTag: "9.9.9", verDigest: "sha256:junk",
			wantKind: model.KindNone, wantRisk: model.RiskNone, wantTo: "3.0.0",
		},
		{
			name: "named channel tag unchanged is up to date",
			c:    model.Container{Tag: "nightly", Digest: "sha256:n1"}, running: "",
			newestTag: "nightly", newestDigest: "sha256:n1", verTag: "3.1.0", verDigest: "sha256:rel",
			wantKind: model.KindNone, wantRisk: model.RiskNone,
		},

		// --- pinned version tags: a newer tag is advice, never an update ---
		{
			name: "wyoming-openai: pinned 0.6.1, 0.7.0 exists, tag unchanged → advisory",
			c:    model.Container{Tag: "0.6.1", Digest: "sha256:7200"}, running: "0.6.1",
			newestTag: "0.7.0", newestDigest: "sha256:7200", verTag: "0.7.0", verDigest: "",
			wantKind: model.KindNone, wantRisk: model.RiskNone, wantNewer: "0.7.0",
		},
		{
			name: "pinned tag current through a mirror digest → advisory",
			c:    model.Container{Tag: "1.2.0", Digest: "sha256:mirror", Digests: []string{"sha256:mirror", "sha256:up"}}, running: "1.2.0",
			newestTag: "2.0.0", newestDigest: "sha256:up",
			wantKind: model.KindNone, wantRisk: model.RiskNone, wantNewer: "2.0.0",
		},
		{
			name: "pinned tag with an unknown running digest: nothing provable → advisory",
			c:    model.Container{Tag: "1.2.0", Digest: ""}, running: "1.2.0",
			newestTag: "1.3.0", newestDigest: "sha256:t",
			wantKind: model.KindNone, wantRisk: model.RiskNone, wantNewer: "1.3.0",
		},
		{
			name: "pinned tag that is already the newest → plain up to date",
			c:    model.Container{Tag: "0.7.0", Digest: "sha256:7200"}, running: "0.7.0",
			newestTag: "0.7.0", newestDigest: "sha256:7200", verTag: "0.7.0",
			wantKind: model.KindNone, wantRisk: model.RiskNone, wantTo: "0.7.0",
		},
		{
			name: "pinned tag whose own image was re-pushed is a real update",
			c:    model.Container{Tag: "1.2.0", Digest: "sha256:old"}, running: "1.2.0",
			newestTag: "1.4.0", newestDigest: "sha256:rebuilt",
			wantKind: model.KindMinor, wantRisk: model.RiskMedium,
		},

		// --- real moves of a floating tag stay updates ---
		{
			name: "SearXNG: latest moved, newest version tag is not that image → plain digest",
			c:    model.Container{Tag: "latest", Digest: "sha256:a07a"}, running: "2026.9.30-a9d990033",
			newestTag: "latest", newestDigest: "sha256:d4c2", verTag: "2025.8.1", verDigest: "sha256:junk",
			wantKind: model.KindDigest, wantRisk: model.RiskLow,
		},
		{
			name: "mealie-style nightly moved, no version tags at all → digest",
			c:    model.Container{Tag: "nightly", Digest: "sha256:5287"}, running: "",
			newestTag: "nightly", newestDigest: "sha256:new",
			wantKind: model.KindDigest, wantRisk: model.RiskLow,
		},
		{
			name: "real latest move whose versioned twin is known → labelled by version delta",
			c:    model.Container{Tag: "latest", Digest: "sha256:run"}, running: "7.1.0",
			newestTag: "latest", newestDigest: "sha256:new", verTag: "7.2.0", verDigest: "sha256:new",
			wantKind: model.KindMinor, wantRisk: model.RiskMedium, wantTo: "7.2.0",
		},
		{
			name: "real latest move, twin is a patch release",
			c:    model.Container{Tag: "latest", Digest: "sha256:run"}, running: "2026.9.0",
			newestTag: "latest", newestDigest: "sha256:new", verTag: "2026.9.1", verDigest: "sha256:new",
			wantKind: model.KindPatch, wantRisk: model.RiskLow, wantTo: "2026.9.1",
		},
		{
			name: "real latest move, twin is a major release",
			c:    model.Container{Tag: "latest", Digest: "sha256:run"}, running: "1.9.4",
			newestTag: "latest", newestDigest: "sha256:new", verTag: "2.0.0", verDigest: "sha256:new",
			wantKind: model.KindMajor, wantRisk: model.RiskHigh, wantTo: "2.0.0",
		},
		{
			name: "real latest move with a junk date tag higher than the twin → junk ignored",
			c:    model.Container{Tag: "latest", Digest: "sha256:run"}, running: "4.0.20",
			newestTag: "latest", newestDigest: "sha256:new", verTag: "2021.12.16", verDigest: "sha256:old2021",
			wantKind: model.KindDigest, wantRisk: model.RiskLow,
		},
		{
			name: "real latest move, twin is the same version as the running label (rebuild)",
			c:    model.Container{Tag: "latest", Digest: "sha256:run"}, running: "7.2.0",
			newestTag: "latest", newestDigest: "sha256:new", verTag: "7.2.0", verDigest: "sha256:new",
			wantKind: model.KindDigest, wantRisk: model.RiskLow, wantTo: "7.2.0",
		},
		{
			name: "real latest move, twin known but running version unknown → digest, notes of the twin",
			c:    model.Container{Tag: "latest", Digest: "sha256:run"}, running: "",
			newestTag: "latest", newestDigest: "sha256:new", verTag: "3.4.5", verDigest: "sha256:new",
			wantKind: model.KindDigest, wantRisk: model.RiskLow, wantTo: "3.4.5",
		},
		{
			name: "digest move with an empty version digest never trusts the version tag",
			c:    model.Container{Tag: "latest", Digest: "sha256:run"}, running: "1.0.0",
			newestTag: "latest", newestDigest: "sha256:new", verTag: "5.0.0", verDigest: "",
			wantKind: model.KindDigest, wantRisk: model.RiskLow,
		},
		{
			name: "unknown running digest on a floating tag claims nothing",
			c:    model.Container{Tag: "latest", Digest: ""}, running: "1.0.0",
			newestTag: "latest", newestDigest: "sha256:new", verTag: "1.1.0", verDigest: "sha256:new",
			wantKind: model.KindNone, wantRisk: model.RiskNone, wantTo: "1.0.0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := classify(tc.c, tc.running, tc.newestTag, tc.newestDigest, tc.verTag, tc.verDigest)
			if v.kind != tc.wantKind || v.risk != tc.wantRisk {
				t.Errorf("kind/risk = %s/%s, want %s/%s (reason %q)", v.kind, v.risk, tc.wantKind, tc.wantRisk, v.reason)
			}
			if v.newer != tc.wantNewer {
				t.Errorf("newer = %q, want %q", v.newer, tc.wantNewer)
			}
			if v.to != tc.wantTo {
				t.Errorf("changelog end = %q, want %q", v.to, tc.wantTo)
			}
		})
	}
}

// End to end through a sweep: the four phantom containers read as up to date
// with no advisory and the changelog is asked for the running version (never
// for the junk tag), while a real digest move stays an update.
func TestSweepPhantomMajorsAreUpToDate(t *testing.T) {
	col := fakeCollector{list: []model.Container{
		{ID: "sonarr", Name: "sonarr", Repo: "lscr.io/linuxserver/sonarr", Tag: "latest", Digest: "sha256:f247", ImageVersion: "4.0.20.3014-ls326", Source: "https://github.com/Sonarr/Sonarr"},
		{ID: "tautulli", Name: "tautulli", Repo: "docker.io/linuxserver/tautulli", Tag: "latest", Digest: "sha256:bfcd", ImageVersion: "v2.18.2-ls246"},
		{ID: "nzbget", Name: "nzbget", Repo: "lscr.io/linuxserver/nzbget", Tag: "latest", Digest: "sha256:ac88", ImageVersion: "v26.3-ls265"},
		{ID: "coral", Name: "CoralHub", Repo: "git.example.net/x/coralhub", Tag: "latest", Digest: "sha256:9638", ImageVersion: "0.0.0-dev"},
		{ID: "searx", Name: "SearXNG", Repo: "docker.io/searxng/searxng", Tag: "latest", Digest: "sha256:a07a", ImageVersion: "2026.9.30-a9d990033"},
	}}
	res := fakeResolver{byRepo: map[string]resolveResult{
		"lscr.io/linuxserver/sonarr":     {tag: "latest", dig: "sha256:f247", verTag: "5.14", verDig: "sha256:v5dev"},
		"docker.io/linuxserver/tautulli": {tag: "latest", dig: "sha256:bfcd", verTag: "2021.12.16", verDig: "sha256:old"},
		"lscr.io/linuxserver/nzbget":     {tag: "latest", dig: "sha256:ac88", verTag: "2021.11.25", verDig: "sha256:old2"},
		"git.example.net/x/coralhub":     {tag: "latest", dig: "sha256:9638", verTag: "1.2.1", verDig: "sha256:other"},
		"docker.io/searxng/searxng":      {tag: "latest", dig: "sha256:d4c2", verTag: "2025.8.1", verDig: "sha256:junk"},
	}}
	st := &fakeStore{}
	e := New(col, res, &fakeChangelog{}, st, time.Hour)
	if err := e.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"sonarr", "tautulli", "nzbget", "coral"} {
		row := st.rows[id]
		if row.HasUpdate() || row.Kind != model.KindNone || row.Risk != model.RiskNone || row.NewerVersion != "" {
			t.Errorf("%s: want plain up to date, got kind=%s risk=%s newer=%q (%s)", id, row.Kind, row.Risk, row.NewerVersion, row.RiskReason)
		}
		cl := row.Changelog
		if cl == nil {
			t.Fatalf("%s: an up-to-date container still gets the running version's notes", id)
		}
		if cl.FromTag != row.RunningVersion || cl.ToTag != row.RunningVersion {
			t.Errorf("%s: changelog span %q → %q, want the running version %q on both ends (never the junk tag)", id, cl.FromTag, cl.ToTag, row.RunningVersion)
		}
	}

	searx := st.rows["searx"]
	if searx.Kind != model.KindDigest || searx.Risk != model.RiskLow {
		t.Errorf("SearXNG: a real digest move must stay an update, got kind=%s risk=%s", searx.Kind, searx.Risk)
	}
	if searx.Changelog == nil || searx.Changelog.ToTag != "latest" {
		t.Errorf("SearXNG: without a trusted twin the changelog asks for the recent releases (to=latest), got %+v", searx.Changelog)
	}
}

// A stale 3.5.1 "major" row for a container that is in fact current must be
// replaced by the up-to-date verdict on the next sweep, and the sweep must not
// announce an update for it.
func TestSweepClearsStalePhantomVerdict(t *testing.T) {
	col := fakeCollector{list: []model.Container{
		{ID: "sonarr", Name: "sonarr", Repo: "lscr.io/linuxserver/sonarr", Tag: "latest", Digest: "sha256:f247", ImageVersion: "4.0.20.3014-ls326"},
	}}
	res := fakeResolver{byRepo: map[string]resolveResult{
		"lscr.io/linuxserver/sonarr": {tag: "latest", dig: "sha256:f247", verTag: "5.14", verDig: "sha256:v5dev"},
	}}
	st := &fakeStore{rows: map[string]model.UpdateStatus{
		"sonarr": {
			Container:    model.Container{ID: "sonarr", Name: "sonarr", Digest: "sha256:f247"},
			NewestDigest: "sha256:f247", NewestTag: "latest",
			Kind: model.KindMajor, Risk: model.RiskHigh, RunningVersion: "4.0.20.3014-ls326",
		},
	}}
	nf := &fakeNotifier{}
	if err := New(col, res, &fakeChangelog{}, st, time.Hour).WithNotifier(nf).Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if row := st.rows["sonarr"]; row.HasUpdate() {
		t.Fatalf("stale major must clear to up to date, got kind=%s", row.Kind)
	}
	if nf.count() != 0 {
		t.Fatalf("an up-to-date verdict must not notify, got %d", nf.count())
	}
}

// wyoming-openai is pinned to :0.6.1 while 0.7.0 exists. Pulling :0.6.1 again
// changes nothing, so it is an advisory: not an update, not notified, with the
// changelog asked for the span a tag change would cross.
func TestSweepPinnedTagNewerVersionIsAdvisory(t *testing.T) {
	col := fakeCollector{list: []model.Container{
		{ID: "wyo", Name: "wyoming-openai", Repo: "ghcr.io/roryeckel/wyoming_openai", Tag: "0.6.1", Digest: "sha256:7200"},
	}}
	res := fakeResolver{byRepo: map[string]resolveResult{
		"ghcr.io/roryeckel/wyoming_openai": {tag: "0.7.0", dig: "sha256:7200", verTag: "0.7.0"},
	}}
	nf := &fakeNotifier{}
	st := &fakeStore{rows: map[string]model.UpdateStatus{
		// previous sweep (also with a prior row, so a notification would be allowed)
		"wyo": {Container: model.Container{ID: "wyo", Name: "wyoming-openai"}, Kind: model.KindNone, Risk: model.RiskNone},
	}}
	e := New(col, res, &fakeChangelog{}, st, time.Hour).WithNotifier(nf)
	if err := e.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	row := st.rows["wyo"]
	if row.Kind != model.KindNone || row.Risk != model.RiskNone || row.HasUpdate() {
		t.Fatalf("pinned tag must not be an update, got kind=%s risk=%s", row.Kind, row.Risk)
	}
	if row.NewerVersion != "0.7.0" || !row.Advisory() {
		t.Fatalf("want advisory for 0.7.0, got newer=%q advisory=%v", row.NewerVersion, row.Advisory())
	}
	if row.RunningVersion != "0.6.1" {
		t.Errorf("running version = %q, want the pinned 0.6.1", row.RunningVersion)
	}
	if row.Changelog == nil || row.Changelog.FromTag != "0.6.1" || row.Changelog.ToTag != "0.7.0" {
		t.Errorf("changelog should cover what changing the tag would bring (0.6.1 → 0.7.0), got %+v", row.Changelog)
	}
	if nf.count() != 0 {
		t.Errorf("an advisory is not an update and must not notify, got %d", nf.count())
	}
}

// A pinned tag that the registry really re-pushed IS something a pull delivers:
// it stays an update, labelled by the version delta as before.
func TestSweepPinnedTagThatMovedStaysUpdate(t *testing.T) {
	col := fakeCollector{list: []model.Container{
		{ID: "p", Name: "pinned", Repo: "ghcr.io/x/pinned", Tag: "1.2.0", Digest: "sha256:old"},
	}}
	res := fakeResolver{byRepo: map[string]resolveResult{
		"ghcr.io/x/pinned": {tag: "1.4.0", dig: "sha256:rebuilt", verTag: "1.4.0"},
	}}
	st := &fakeStore{}
	if err := New(col, res, &fakeChangelog{}, st, time.Hour).Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	row := st.rows["p"]
	if row.Kind != model.KindMinor || row.NewerVersion != "" {
		t.Fatalf("a moved pinned tag is a real update with no advisory, got kind=%s newer=%q", row.Kind, row.NewerVersion)
	}
}

// A real move whose new image is the newest version tag is labelled by the
// version delta and the changelog covers exactly that span.
func TestSweepRealMoveWithTwinUsesTwinForChangelog(t *testing.T) {
	col := fakeCollector{list: []model.Container{
		{ID: "oc", Name: "opencloud", Repo: "ghcr.io/o/opencloud", Tag: "latest", Digest: "sha256:run", ImageVersion: "7.1.0"},
	}}
	res := fakeResolver{byRepo: map[string]resolveResult{
		"ghcr.io/o/opencloud": {tag: "latest", dig: "sha256:new", verTag: "7.2.0", verDig: "sha256:new"},
	}}
	st := &fakeStore{}
	if err := New(col, res, &fakeChangelog{}, st, time.Hour).Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	row := st.rows["oc"]
	if row.Kind != model.KindMinor {
		t.Fatalf("kind = %s, want minor", row.Kind)
	}
	if cl := row.Changelog; cl == nil || cl.FromTag != "7.1.0" || cl.ToTag != "7.2.0" {
		t.Fatalf("changelog span = %+v, want 7.1.0 → 7.2.0", cl)
	}
}

// The whole chain on the real cases, as the daily auto-update sees it: sweep the
// fleet, then ask the policy (level major, digest moves on — the production
// setting) which containers it would touch. Only genuinely newer images are
// eligible; the phantoms and the pinned container are not; Cody stays excluded.
func TestRealFleetEligibility(t *testing.T) {
	col := fakeCollector{list: []model.Container{
		{ID: "sonarr", Name: "sonarr", Repo: "r/sonarr", Tag: "latest", Digest: "sha256:s1", ImageVersion: "4.0.20.3014-ls326"},
		{ID: "tautulli", Name: "tautulli", Repo: "r/tautulli", Tag: "latest", Digest: "sha256:t1", ImageVersion: "v2.18.2-ls246"},
		{ID: "nzbget", Name: "nzbget", Repo: "r/nzbget", Tag: "latest", Digest: "sha256:n1", ImageVersion: "v26.3-ls265"},
		{ID: "coral", Name: "CoralHub", Repo: "r/coralhub", Tag: "latest", Digest: "sha256:c1", ImageVersion: "0.0.0-dev"},
		{ID: "wyo", Name: "wyoming-openai", Repo: "r/wyoming", Tag: "0.6.1", Digest: "sha256:w1"},
		{ID: "redis", Name: "Redis", Repo: "r/redis", Tag: "latest", Digest: "sha256:r1"},
		{ID: "searx", Name: "SearXNG", Repo: "r/searxng", Tag: "latest", Digest: "sha256:x-old", ImageVersion: "2026.9.30-a9d990033"},
		{ID: "mealie", Name: "mealiev1", Repo: "r/mealie", Tag: "nightly", Digest: "sha256:m-old"},
		{ID: "oc", Name: "opencloud", Repo: "r/opencloud", Tag: "latest", Digest: "sha256:o-old", ImageVersion: "7.1.0"},
		{ID: "cody", Name: "Cody", Repo: "r/cody", Tag: "latest", Digest: "sha256:cody-old", ImageVersion: "0.42.1"},
	}}
	res := fakeResolver{byRepo: map[string]resolveResult{
		// phantoms: own tag unchanged, the registry's highest version tag is junk
		"r/sonarr":   {tag: "latest", dig: "sha256:s1", verTag: "5.14", verDig: "sha256:v5"},
		"r/tautulli": {tag: "latest", dig: "sha256:t1", verTag: "2021.12.16", verDig: "sha256:d1"},
		"r/nzbget":   {tag: "latest", dig: "sha256:n1", verTag: "2021.11.25", verDig: "sha256:d2"},
		"r/coralhub": {tag: "latest", dig: "sha256:c1", verTag: "1.2.1", verDig: "sha256:o1"},
		// pinned: 0.7.0 exists, :0.6.1 is unchanged
		"r/wyoming": {tag: "0.7.0", dig: "sha256:w1", verTag: "0.7.0"},
		"r/redis":   {tag: "latest", dig: "sha256:r1"},
		// real moves
		"r/searxng":   {tag: "latest", dig: "sha256:x-new", verTag: "2025.8.1", verDig: "sha256:junk"},
		"r/mealie":    {tag: "nightly", dig: "sha256:m-new"},
		"r/opencloud": {tag: "latest", dig: "sha256:o-new", verTag: "7.2.0", verDig: "sha256:o-new"},
		"r/cody":      {tag: "latest", dig: "sha256:cody-new"},
	}}
	st := &fakeStore{}
	if err := New(col, res, &fakeChangelog{}, st, time.Hour).Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, _ := st.List()

	policy := autoupdate.Policy{Level: autoupdate.LevelMajor, Digest: true, ExcludeContainers: []string{"Cody"}}
	var eligible, skipped []string
	for _, row := range rows {
		if !autoupdate.Eligible(row, policy) {
			continue
		}
		if autoupdate.ContainerExcluded(row.Container.Name, policy.ExcludeContainers) {
			skipped = append(skipped, row.Container.Name)
			continue
		}
		eligible = append(eligible, row.Container.Name)
	}
	sort.Strings(eligible)
	if want := []string{"SearXNG", "mealiev1", "opencloud"}; !reflect.DeepEqual(eligible, want) {
		t.Errorf("eligible = %v, want %v (only genuinely newer images)", eligible, want)
	}
	if want := []string{"Cody"}; !reflect.DeepEqual(skipped, want) {
		t.Errorf("skipped as excluded = %v, want %v", skipped, want)
	}
	if row := st.rows["wyo"]; !row.Advisory() || row.NewerVersion != "0.7.0" {
		t.Errorf("wyoming-openai should be an advisory for 0.7.0, got %+v", row)
	}
	if row := st.rows["oc"]; row.Kind != model.KindMinor {
		t.Errorf("opencloud's twin makes it a minor update, got %s", row.Kind)
	}
}
