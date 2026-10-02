package cafeed

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- Lookup: pure matching logic, no network ---

func feedFrom(entries []Entry, blacklisted map[string]string, previous *Feed) *Feed {
	byName, byRepo, byTemplateURL := indexEntries(entries)
	return &Feed{
		byName: byName, byRepo: byRepo, byTemplateURL: byTemplateURL,
		templateRepos: indexTemplateRepos(entries, nil),
		blacklisted:   blacklisted, previous: previous,
	}
}

func TestLookupListedAndHealthy(t *testing.T) {
	f := feedFrom([]Entry{{Name: "OpenCloud", Repository: "junkerderprovinz/opencloud"}}, nil, nil)
	res, ok := f.Lookup("OpenCloud", "docker.io/junkerderprovinz/opencloud", "")
	if !ok || !res.Listed || res.Deprecated {
		t.Fatalf("want listed/healthy/ok, got %+v ok=%v", res, ok)
	}
}

// Reproduces the real coppit/handbrake feed entry found live: Deprecated with
// a moderator comment, while still genuinely listed (not absent).
func TestLookupDeprecatedIsNotAbsent(t *testing.T) {
	f := feedFrom([]Entry{{
		Name: "HandBrake", Repository: "coppit/handbrake",
		Deprecated: true, ModeratorComment: "A better supported and more up to date app is available from DJoss",
	}}, nil, nil)
	res, ok := f.Lookup("HandBrake", "docker.io/coppit/handbrake", "")
	if !ok {
		t.Fatal("want ok")
	}
	if !res.Listed {
		t.Error("a deprecated (but not removed) app must still read Listed=true — it's an editorial demotion, not a dead end")
	}
	if !res.Deprecated || res.Note == "" {
		t.Errorf("want Deprecated=true with a note, got %+v", res)
	}
}

func TestLookupAbsentWithNoPreviousCrawlIsInconclusive(t *testing.T) {
	f := feedFrom([]Entry{{Name: "OtherApp", Repository: "x/y"}}, nil, nil)
	_, ok := f.Lookup("GoneApp", "x/gone", "")
	if ok {
		t.Fatal("an absence with no previous crawl to confirm against must be inconclusive (ok=false)")
	}
}

func TestLookupAbsentPresentInPreviousCrawlIsInconclusive(t *testing.T) {
	prev := feedFrom([]Entry{{Name: "FlakyApp", Repository: "x/flaky"}}, nil, nil)
	cur := feedFrom([]Entry{{Name: "OtherApp", Repository: "x/y"}}, nil, prev)
	_, ok := cur.Lookup("FlakyApp", "x/flaky", "")
	if ok {
		t.Fatal("present last crawl, absent this one, must be inconclusive — a transient crawl gap, not confirmed removal (the exact false-positive shape already fixed once for the raw-URL proxy)")
	}
}

// An app whose template lives in a repository CA crawls (here: a sibling
// template from the same repo is still listed) and which is missing from both
// crawls really was pulled.
func TestLookupAbsentConfirmedAcrossTwoCrawls(t *testing.T) {
	sibling := Entry{Name: "OtherApp", Repository: "x/y", TemplateURL: "https://raw.githubusercontent.com/x/tpl/main/other.xml"}
	prev := feedFrom([]Entry{sibling}, nil, nil)
	cur := feedFrom([]Entry{sibling}, nil, prev)
	res, ok := cur.Lookup("TrulyGoneApp", "x/gone", "https://raw.githubusercontent.com/x/tpl/main/gone.xml")
	if !ok || res.Listed {
		t.Fatalf("absent from both crawls must be confirmed not-listed, got %+v ok=%v", res, ok)
	}
}

// Reproduces a live false positive: a user's own app (own image, own template
// in their own GitHub repo, never submitted to CA) is absent from every crawl
// by nature. Absence from a repository CA does not crawl proves nothing, so
// the lookup must stay inconclusive — never "removed from Community
// Applications".
func TestLookupAbsentFromUncrawledTemplateRepoIsInconclusive(t *testing.T) {
	sibling := Entry{Name: "OtherApp", Repository: "x/y", TemplateURL: "https://raw.githubusercontent.com/x/tpl/main/other.xml"}
	prev := feedFrom([]Entry{sibling}, nil, nil)
	cur := feedFrom([]Entry{sibling}, nil, prev)
	for _, tmpl := range []string{
		"https://raw.githubusercontent.com/me/own-app/main/unraid/own-app.xml", // own public repo
		"https://raw.githubusercontent.com/me/private-templates/main/x.xml",    // own private repo
		"https://git.example.org/me/app/raw/branch/main/app.xml",               // not a host CA crawls
		"", // hand-authored template, no TemplateURL
	} {
		if res, ok := cur.Lookup("OwnApp", "ghcr.io/me/own-app", tmpl); ok {
			t.Errorf("template %q: absent from a repo CA never crawled must be inconclusive, got %+v", tmpl, res)
		}
	}
}

func TestLookupBlacklisted(t *testing.T) {
	f := feedFrom(
		[]Entry{{Name: "BadApp", Repository: "x/bad"}},
		map[string]string{"x/bad": "Repository no longer exists on dockerHub"},
		nil,
	)
	res, ok := f.Lookup("BadApp", "docker.io/x/bad", "")
	if !ok || res.Listed || res.Note == "" {
		t.Fatalf("blacklisted must read not-listed with a reason, got %+v ok=%v", res, ok)
	}
}

// Two maintainers publish an app under the same display Name (the real
// HandBrake case) — must disambiguate by the container's own repository,
// not just grab the first match.
func TestLookupAmbiguousNameDisambiguatedByRepo(t *testing.T) {
	f := feedFrom([]Entry{
		{Name: "HandBrake", Repository: "coppit/handbrake", Deprecated: true, ModeratorComment: "superseded"},
		{Name: "HandBrake", Repository: "jlesage/handbrake"},
	}, nil, nil)
	res, ok := f.Lookup("HandBrake", "docker.io/jlesage/handbrake", "")
	if !ok {
		t.Fatal("want ok")
	}
	if res.Deprecated {
		t.Error("matched the wrong maintainer's entry — jlesage's is not deprecated, coppit's is")
	}
}

func TestLookupAmbiguousNameDisambiguatedByTemplateURL(t *testing.T) {
	f := feedFrom([]Entry{
		{Name: "SameName", Repository: "a/x", TemplateURL: "https://raw.githubusercontent.com/a/tpl/main/x.xml"},
		{Name: "SameName", Repository: "b/x", TemplateURL: "https://raw.githubusercontent.com/b/tpl/main/x.xml", Deprecated: true},
	}, nil, nil)
	// repo doesn't narrow it (empty), templateURL must.
	res, ok := f.Lookup("SameName", "", "https://raw.githubusercontent.com/b/tpl/main/x.xml")
	if !ok || !res.Deprecated {
		t.Fatalf("want the b/x entry via templateURL match, got %+v ok=%v", res, ok)
	}
}

func TestLookupAmbiguousUnresolvableIsInconclusive(t *testing.T) {
	f := feedFrom([]Entry{
		{Name: "SameName", Repository: "a/x"},
		{Name: "SameName", Repository: "b/x"},
	}, nil, nil)
	_, ok := f.Lookup("SameName", "docker.io/c/x", "")
	if ok {
		t.Fatal("neither candidate's repository matches the container's — must be inconclusive, not a guess")
	}
}

func TestNormalizeRepoStripsAnyRegistryHost(t *testing.T) {
	cases := map[string]string{
		"docker.io/library/redis":                      "redis",
		"docker.io/coppit/handbrake":                   "coppit/handbrake",
		"ghcr.io/binhex/arch-teamspeak":                "binhex/arch-teamspeak", // must compare equal to the docker.io form below
		"binhex/arch-teamspeak":                        "binhex/arch-teamspeak",
		"quay.io/prometheus/prometheus":                "prometheus/prometheus",
		"ghcr.io/open-webui/open-webui:main":           "open-webui/open-webui", // CA's Repository field observed live with a baked-in tag
		"docker.openhands.dev/openhands/openhands:1.7": "openhands/openhands",
		"redis:latest":                                 "redis",
	}
	for in, want := range cases {
		if got := normalizeRepo(in); got != want {
			t.Errorf("normalizeRepo(%q) = %q, want %q", in, got, want)
		}
	}
}

// Reproduces the real OpenWebUI case found live: CA's Repository field bakes
// in a ":main" tag the running container's own (tag-less) Repo never has.
func TestLookupZeroNameMatchRescuedByRepoDespiteFeedTagSuffix(t *testing.T) {
	f := feedFrom([]Entry{
		{Name: "open-webui", Repository: "ghcr.io/open-webui/open-webui:main"},
	}, nil, nil)
	res, ok := f.Lookup("OpenWebUI", "ghcr.io/open-webui/open-webui", "")
	if !ok || !res.Listed {
		t.Fatalf("a feed Repository with a baked-in tag must still match the container's tag-less repo, got %+v ok=%v", res, ok)
	}
}

// --- Lookup: zero-name-match rescue by repo/templateURL ---

// Reproduces the real TeamSpeak case found live: the container is renamed
// away from its CA template's canonical Name ("TeamSpeak" vs the feed's
// "binhex-teamspeak"), and CA references the app via its ghcr.io mirror while
// the container runs the docker.io original. Neither is a removal.
func TestLookupZeroNameMatchRescuedByRepo(t *testing.T) {
	f := feedFrom([]Entry{
		{Name: "binhex-teamspeak", Repository: "ghcr.io/binhex/arch-teamspeak"},
	}, nil, nil)
	res, ok := f.Lookup("TeamSpeak", "docker.io/binhex/arch-teamspeak", "")
	if !ok || !res.Listed {
		t.Fatalf("a renamed container must still resolve via its repository, got %+v ok=%v", res, ok)
	}
}

// A multi-instance container (user-suffixed "-II", "-III", ...) shares one
// CA template's repository under a name the feed never lists.
func TestLookupZeroNameMatchMultiInstanceRescuedByRepo(t *testing.T) {
	f := feedFrom([]Entry{
		{Name: "storj", Repository: "storjlabs/storagenode"},
	}, nil, nil)
	res, ok := f.Lookup("Storj-III", "docker.io/storjlabs/storagenode", "")
	if !ok || !res.Listed {
		t.Fatalf("a suffixed multi-instance container must still resolve via its repository, got %+v ok=%v", res, ok)
	}
}

func TestLookupZeroNameMatchRescuedByTemplateURL(t *testing.T) {
	f := feedFrom([]Entry{
		{Name: "some-canonical-name", TemplateURL: "https://raw.githubusercontent.com/a/tpl/main/x.xml"},
	}, nil, nil)
	res, ok := f.Lookup("MyRenamedApp", "", "https://raw.githubusercontent.com/a/tpl/main/x.xml")
	if !ok || !res.Listed {
		t.Fatalf("a renamed container with no repo signal must still resolve via its installed TemplateURL, got %+v ok=%v", res, ok)
	}
}

// The rescue must narrow false positives, not blind true removals: an app
// absent by name AND repository across two crawls is still confirmed gone.
func TestLookupZeroNameMatchStillAbsentWhenRepoAlsoMisses(t *testing.T) {
	sibling := Entry{Name: "OtherApp", Repository: "x/y", TemplateURL: "https://raw.githubusercontent.com/x/tpl/main/other.xml"}
	prev := feedFrom([]Entry{sibling}, nil, nil)
	cur := feedFrom([]Entry{sibling}, nil, prev)
	res, ok := cur.Lookup("TrulyGoneApp", "x/gone-for-real", "https://raw.githubusercontent.com/x/tpl/main/gone.xml")
	if !ok || res.Listed {
		t.Fatalf("absent by name AND repository from both crawls must still be confirmed not-listed, got %+v ok=%v", res, ok)
	}
}

func TestLookupZeroNameMatchRepoPresentLastCrawlIsInconclusive(t *testing.T) {
	prev := feedFrom([]Entry{{Name: "flaky-name", Repository: "x/flaky"}}, nil, nil)
	cur := feedFrom([]Entry{{Name: "OtherApp", Repository: "x/y"}}, nil, prev)
	_, ok := cur.Lookup("RenamedFlakyApp", "x/flaky", "")
	if ok {
		t.Fatal("present last crawl by repository, absent by both name and repo this crawl, must be inconclusive — a transient crawl gap")
	}
}

// --- CrawlsTemplate: is this template one Community Applications publishes? ---

func TestTemplateRepoKey(t *testing.T) {
	cases := map[string]string{
		"https://raw.githubusercontent.com/Owner/Repo/main/dir/app.xml":        "github.com/owner/repo",
		"https://raw.githubusercontent.com/o/r/refs/heads/main/app.xml":        "github.com/o/r",
		"https://github.com/o/r/blob/master/untelegraf.xml":                    "github.com/o/r", // a blob URL some installs store as TemplateURL
		"https://github.com/o/r":                                               "github.com/o/r",
		"https://github.com/o/r.git":                                           "github.com/o/r",
		"https://www.github.com/o/r/":                                          "github.com/o/r",
		"http://raw.githubusercontent.com/o/r/main/a.xml?x=1#frag":             "github.com/o/r",
		"https://gitlab.com/yaya/unraid-templates/-/raw/main/yaya/frigate.xml": "gitlab.com/yaya/unraid-templates",
		"https://gitlab.com/group/sub/proj/-/raw/master/docker/unraid.xml":     "gitlab.com/group/sub/proj", // nested group
		"https://gitlab.com/owner/repo/raw/master/app.xml":                     "gitlab.com/owner/repo",     // pre-"/-/" raw URL
		"https://gitlab.com/group/sub/proj":                                    "gitlab.com/group/sub/proj", // a bare project URL, as the repositories list stores it
		"https://gitlab.com/group/sub/proj/":                                   "gitlab.com/group/sub/proj",
		"https://gitlab.com/owner/repo/blob/main/dir/app.xml":                  "gitlab.com/owner/repo",
		"https://gitlab.com/owner/repo.git":                                    "gitlab.com/owner/repo",
		"https://raw.githubusercontent.com/onlyowner":                          "",
		"https://git.example.org/me/app/raw/branch/main/app.xml":               "", // not a host CA crawls
		"https://gist.githubusercontent.com/me/abc123/raw/app.xml":             "",
		"ftp://github.com/o/r":                                                 "",
		"not a url":                                                            "",
		"":                                                                     "",
	}
	for in, want := range cases {
		got, ok := templateRepoKey(in)
		if want == "" {
			if ok {
				t.Errorf("templateRepoKey(%q) = %q, want no repository", in, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("templateRepoKey(%q) = %q,%v want %q", in, got, ok, want)
		}
	}
}

// A feed shaped like the real one: an explicit repositories list plus apps
// that point into those repositories.
const realisticFeed = `{
  "applist": [
    {"Name":"actual-server","Repository":"actualbudget/actual-server","TemplateURL":"https://raw.githubusercontent.com/hofq/docker-templates/main/actual-server.xml"},
    {"Name":"frigate","Repository":"ghcr.io/blakeblackshear/frigate","TemplateURL":"https://gitlab.com/yayitazale/unraid-templates/-/raw/main/yayitazale/frigate.xml"}
  ],
  "repositories": {
    "hofq's Repository": {"url":"https://github.com/hofq/docker-templates","bio":"x"},
    "Empty Repo":        {"url":"https://github.com/someone/emptied-repo"},
    "yayitazale":        {"url":"https://gitlab.com/yayitazale/unraid-templates"},
    "Nested":            {"url":"https://gitlab.com/group/sub/proj"}
  },
  "blacklisted": {"x/bad":"Repository no longer exists on dockerHub"},
  "last_updated_timestamp": 5
}`

func TestCrawlsTemplate(t *testing.T) {
	f := parseOne([]byte(realisticFeed))
	if f == nil {
		t.Fatal("realistic feed must parse")
	}
	cases := []struct {
		name, url string
		want      bool
	}{
		{"another template in a listed repo", "https://raw.githubusercontent.com/hofq/docker-templates/main/other-app.xml", true},
		{"repo listed by name, case differs", "https://raw.githubusercontent.com/HOFQ/Docker-Templates/main/a.xml", true},
		{"repo in the repositories list with no app left in the feed", "https://raw.githubusercontent.com/someone/emptied-repo/main/a.xml", true},
		{"gitlab repo", "https://gitlab.com/yayitazale/unraid-templates/-/raw/main/yayitazale/other.xml", true},
		{"nested gitlab project listed only in the repositories list", "https://gitlab.com/group/sub/proj/-/raw/main/app.xml", true},
		{"the user's own repo, never submitted to CA", "https://raw.githubusercontent.com/me/own-app/main/unraid/own-app.xml", false},
		{"a lookalike owner", "https://raw.githubusercontent.com/hofq/docker-templates-fork/main/a.xml", false},
		{"a host CA does not crawl", "https://git.example.org/hofq/docker-templates/raw/branch/main/a.xml", false},
		{"no TemplateURL at all", "", false},
	}
	for _, c := range cases {
		if got := f.CrawlsTemplate(c.url); got != c.want {
			t.Errorf("%s: CrawlsTemplate(%q) = %v, want %v", c.name, c.url, got, c.want)
		}
	}
}

// The repositories list is an extra signal, not a load-bearing one: if its
// shape ever changes the feed must still parse and keep working from the
// listed apps' own TemplateURLs (losing only the "repo with no apps" case).
func TestParseOneSurvivesReshapedRepositories(t *testing.T) {
	f := parseOne([]byte(`{"applist":[{"Name":"A","Repository":"x/a","TemplateURL":"https://raw.githubusercontent.com/hofq/docker-templates/main/a.xml"}],"repositories":["not","a","map"],"last_updated_timestamp":1}`))
	if f == nil {
		t.Fatal("a reshaped repositories field must not make the whole feed unparseable")
	}
	if !f.CrawlsTemplate("https://raw.githubusercontent.com/hofq/docker-templates/main/b.xml") {
		t.Error("the repository behind a listed app's TemplateURL must still count as crawled")
	}
	if res, ok := f.Lookup("A", "x/a", ""); !ok || !res.Listed {
		t.Errorf("a listed app must still resolve, got %+v ok=%v", res, ok)
	}
}

// End to end on a realistic feed: a template from a repo CA crawls that has
// vanished across two crawls is removed; a user's own app, absent the same way,
// is not; and a feed with no previous crawl never condemns anything.
func TestRealisticFeedRemovedVersusNeverListed(t *testing.T) {
	prev := parseOne([]byte(realisticFeed))
	cur := parseOne([]byte(realisticFeed))
	cur.previous = prev

	res, ok := cur.Lookup("GoneApp", "docker.io/gone/app", "https://raw.githubusercontent.com/hofq/docker-templates/main/gone-app.xml")
	if !ok || res.Listed {
		t.Errorf("an app from a crawled repo, absent from both crawls, must read removed; got %+v ok=%v", res, ok)
	}
	if res, ok = cur.Lookup("OwnApp", "ghcr.io/me/own-app", "https://raw.githubusercontent.com/me/own-app/main/unraid/own-app.xml"); ok {
		t.Errorf("a user's own app must never get a verdict from the feed; got %+v", res)
	}
	cur.previous = nil
	if res, ok = cur.Lookup("GoneApp", "docker.io/gone/app", "https://raw.githubusercontent.com/hofq/docker-templates/main/gone-app.xml"); ok {
		t.Errorf("with no previous crawl an absence must stay inconclusive; got %+v", res)
	}
}

// --- Fetcher: HTTP + on-disk cache behaviour ---

const miniFeedTS1 = `{"applist":[{"Name":"App","Repository":"x/app"}],"last_updated_timestamp":1}`
const miniFeedTS2 = `{"applist":[{"Name":"App","Repository":"x/app"},{"Name":"NewApp","Repository":"x/new"}],"last_updated_timestamp":2}`

func testServer(t *testing.T, feedBody string, ts string, fetchCalls *int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/lastUpdated", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"last_updated_timestamp":` + ts + `}`))
	})
	mux.HandleFunc("/feed", func(w http.ResponseWriter, r *http.Request) {
		if fetchCalls != nil {
			*fetchCalls++
		}
		_, _ = w.Write([]byte(feedBody))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestFetcherLoadFetchesOnFirstCall(t *testing.T) {
	dir := t.TempDir()
	srv := testServer(t, miniFeedTS1, "1", nil)
	f := NewFetcher(dir)
	f.feedURL, f.lastUpdatedURL = srv.URL+"/feed", srv.URL+"/lastUpdated"

	feed, err := f.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := feed.Lookup("App", "x/app", ""); !ok {
		t.Fatal("freshly fetched feed should contain the seeded entry")
	}
	if _, err := os.Stat(filepath.Join(dir, cacheFile)); err != nil {
		t.Errorf("expected the feed to be cached to disk: %v", err)
	}
}

func TestFetcherLoadSkipsRefetchWhenTimestampUnchanged(t *testing.T) {
	dir := t.TempDir()
	var fullFetches int
	srv := testServer(t, miniFeedTS1, "1", &fullFetches)
	f := NewFetcher(dir)
	f.feedURL, f.lastUpdatedURL = srv.URL+"/feed", srv.URL+"/lastUpdated"

	if _, err := f.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fullFetches != 1 {
		t.Fatalf("want exactly 1 full-feed fetch across two Loads with an unchanged timestamp, got %d", fullFetches)
	}
}

func TestFetcherLoadRefetchesWhenTimestampChanges(t *testing.T) {
	dir := t.TempDir()
	mux := http.NewServeMux()
	ts := "1"
	body := miniFeedTS1
	mux.HandleFunc("/lastUpdated", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"last_updated_timestamp":` + ts + `}`))
	})
	mux.HandleFunc("/feed", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	f := NewFetcher(dir)
	f.feedURL, f.lastUpdatedURL = srv.URL+"/feed", srv.URL+"/lastUpdated"

	if _, err := f.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	ts, body = "2", miniFeedTS2
	feed, err := f.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := feed.Lookup("NewApp", "x/new", ""); !ok {
		t.Fatal("second Load should have refetched and picked up the new entry")
	}
	// And the previous crawl must now be available for absence cross-checks.
	if feed.previous == nil {
		t.Fatal("expected the prior crawl to be cached as .previous after a refetch")
	}
}

func TestFetcherLoadFallsBackToCacheOnFetchFailure(t *testing.T) {
	dir := t.TempDir()
	srv := testServer(t, miniFeedTS1, "1", nil)
	f := NewFetcher(dir)
	f.feedURL, f.lastUpdatedURL = srv.URL+"/feed", srv.URL+"/lastUpdated"
	if _, err := f.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv.Close() // upstream now unreachable

	feed, err := f.Load(context.Background())
	if err != nil {
		t.Fatalf("a fetch failure with a cache on disk must degrade gracefully, not error: %v", err)
	}
	if _, ok := feed.Lookup("App", "x/app", ""); !ok {
		t.Fatal("fallback feed should still contain the previously cached entry")
	}
}

func TestFetcherLoadErrorsOnlyWhenNothingCachedAtAll(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	f := NewFetcher(dir)
	f.feedURL, f.lastUpdatedURL = srv.URL+"/feed", srv.URL+"/lastUpdated"

	_, err := f.Load(context.Background())
	if err == nil {
		t.Fatal("want an error: fetch failed AND nothing was ever cached — genuinely nothing to check against")
	}
	if !strings.Contains(err.Error(), "cafeed") {
		t.Errorf("error should be identifiable as coming from cafeed, got: %v", err)
	}
}
