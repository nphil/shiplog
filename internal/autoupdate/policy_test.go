package autoupdate

import (
	"testing"

	"github.com/junkerderprovinz/shiplog/internal/model"
)

func st(k model.Kind) model.UpdateStatus { return model.UpdateStatus{Kind: k} }

// advisory is a pinned-tag advisory status: up to date (Kind none) with a newer
// version tag the container cannot reach by pulling its own tag.
func advisory(newer string) model.UpdateStatus {
	return model.UpdateStatus{Kind: model.KindNone, Risk: model.RiskNone, NewerVersion: newer}
}

func TestEligible(t *testing.T) {
	cases := []struct {
		name string
		st   model.UpdateStatus
		p    Policy
		want bool
	}{
		{"patch under patch", st(model.KindPatch), Policy{Level: LevelPatch}, true},
		{"minor under patch", st(model.KindMinor), Policy{Level: LevelPatch}, false},
		{"minor under minor", st(model.KindMinor), Policy{Level: LevelMinor}, true},
		{"major under minor", st(model.KindMajor), Policy{Level: LevelMinor}, false},
		{"major under major", st(model.KindMajor), Policy{Level: LevelMajor}, true},
		{"digest off", st(model.KindDigest), Policy{Level: LevelMajor}, false},
		{"digest on", st(model.KindDigest), Policy{Level: LevelOff, Digest: true}, true},
		{"unknown never", st(model.KindUnknown), Policy{Level: LevelMajor, Digest: true}, false},
		{"none never", st(model.KindNone), Policy{Level: LevelMajor, Digest: true}, false},
		{"off nothing", st(model.KindPatch), Policy{Level: LevelOff}, false},
		// wyoming-openai: pinned to :0.6.1 while 0.7.0 exists. Nothing can be pulled
		// for a pinned tag, so it is advisory information and never auto-applied,
		// whatever the level and with or without the digest toggle.
		{"pinned advisory at major+digest", advisory("0.7.0"), Policy{Level: LevelMajor, Digest: true}, false},
		{"pinned advisory at patch", advisory("0.7.0"), Policy{Level: LevelPatch}, false},
	}
	for _, c := range cases {
		if got := Eligible(c.st, c.p); got != c.want {
			t.Errorf("%s: Eligible = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestParseExcludeWords(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"breaking", []string{"breaking"}},
		{"breaking, migration required", []string{"breaking", "migration required"}},
		{" breaking ,, BREAKING CHANGE , ", []string{"breaking", "BREAKING CHANGE"}},
	}
	for _, c := range cases {
		got := ParseExcludeWords(c.in)
		if len(got) != len(c.want) {
			t.Errorf("ParseExcludeWords(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("ParseExcludeWords(%q) = %v, want %v", c.in, got, c.want)
				break
			}
		}
	}
}

func TestMatchedExcludeWord(t *testing.T) {
	words := []string{"breaking", "Migration Required"}
	cases := []struct {
		name string
		cl   *model.Changelog
		want string
	}{
		{"nil changelog", nil, ""},
		{"empty raw", &model.Changelog{Raw: ""}, ""},
		{"no match", &model.Changelog{Raw: "Bug fixes and performance improvements."}, ""},
		{"exact word", &model.Changelog{Raw: "This release includes a BREAKING change to the API."}, "breaking"},
		{"case-insensitive against mixed-case configured word", &model.Changelog{Raw: "please read the migration required section"}, "Migration Required"},
		{"substring within a larger word still counts", &model.Changelog{Raw: "a groundbreaking new feature"}, "breaking"},
		{"first configured word wins when both match", &model.Changelog{Raw: "breaking change, migration required"}, "breaking"},
	}
	for _, c := range cases {
		if got := MatchedExcludeWord(c.cl, words); got != c.want {
			t.Errorf("%s: MatchedExcludeWord = %q, want %q", c.name, got, c.want)
		}
	}
	if got := MatchedExcludeWord(&model.Changelog{Raw: "breaking"}, nil); got != "" {
		t.Errorf("no configured words must never match, got %q", got)
	}
}

func TestParseLevel(t *testing.T) {
	for s, want := range map[string]Level{
		"off": LevelOff, "patch": LevelPatch, "minor": LevelMinor, "major": LevelMajor,
		"bogus": LevelOff, "": LevelOff,
	} {
		if got := ParseLevel(s); got != want {
			t.Errorf("ParseLevel(%q) = %d, want %d", s, got, want)
		}
	}
}

func TestParseExcludeContainers(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"Cody", []string{"Cody"}},
		{"Cody,plex", []string{"Cody", "plex"}},
		{" Cody ,, plex , ", []string{"Cody", "plex"}},
		{"Cody,cody,CODY,plex", []string{"Cody", "plex"}}, // repeats ignoring case collapse; first spelling wins
	}
	for _, c := range cases {
		got := ParseExcludeContainers(c.in)
		if len(got) != len(c.want) {
			t.Errorf("ParseExcludeContainers(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("ParseExcludeContainers(%q) = %v, want %v", c.in, got, c.want)
				break
			}
		}
	}
}

func TestContainerExcluded(t *testing.T) {
	list := []string{"Cody", "plex"}
	cases := []struct {
		name string
		list []string
		want bool
	}{
		{"Cody", list, true},
		{"cody", list, true}, // case-insensitive both ways
		{"CODY", list, true},
		{"PLEX", list, true},
		{"sonarr", list, false},
		{"Cody2", list, false}, // whole-name match, never a prefix or substring
		{"Cod", list, false},
		{"", list, false},
		{"Cody", nil, false}, // empty list excludes nothing
		{"Cody", []string{}, false},
	}
	for _, c := range cases {
		if got := ContainerExcluded(c.name, c.list); got != c.want {
			t.Errorf("ContainerExcluded(%q, %v) = %v, want %v", c.name, c.list, got, c.want)
		}
	}
}
