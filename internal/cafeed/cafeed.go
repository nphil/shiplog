// Package cafeed checks whether an installed app is still listed in Unraid's
// Community Applications catalog — the same feed CA's own plugin consults
// (Squidly271/AppFeed, mirrored to assets.ca.unraid.net), not the raw
// template-URL reachability proxy engine.repoGone corroborates. That proxy
// only ever catches a genuinely deleted/moved template file; it cannot see
// an app editorially demoted (CA's own "Deprecated" flag: hidden from
// default search, still installable, still updated by its maintainer) or
// pulled from the feed while its template file happens to still sit
// untouched in its source repo.
//
// Fetches are cheap by design: a tiny "-lastUpdated.json" ping is checked
// first, and the ~24MB full feed is only re-downloaded when its timestamp
// actually changed (observed cadence: every 4-8h). Both the current and the
// PREVIOUS successfully-fetched feed are kept on disk — a container is only
// ever reported absent when it is missing from both, since the feed is
// itself crowd-crawled and can have a transient gap for a renamed/moved
// template, the same false-positive shape already fixed once for the raw
// template-URL proxy (see engine.repoGone). Any fetch failure falls back to
// whatever is cached; with nothing cached at all, every lookup is
// inconclusive rather than guessed — fail open, never fail closed.
//
// Absence from the feed only means "removed" for an app CA once carried. An
// app that never was in CA — the user's own image with the user's own
// template — is absent by nature, so the engine asks CrawlsTemplate first and
// leaves every CA verdict alone for a template whose repository CA does not
// crawl.
package cafeed

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	defaultFeedURL        = "https://raw.githubusercontent.com/Squidly271/AppFeed/master/applicationFeed.json"
	defaultLastUpdatedURL = "https://raw.githubusercontent.com/Squidly271/AppFeed/master/applicationFeed-lastUpdated.json"
	cacheFile             = "ca-feed.json"
	prevCacheFile         = "ca-feed.prev.json"
)

// Entry is one app listing from the feed — only the fields ShipLog needs.
type Entry struct {
	Name             string `json:"Name"`
	Repository       string `json:"Repository"`
	TemplateURL      string `json:"TemplateURL"`
	Deprecated       bool   `json:"Deprecated"`
	ModeratorComment string `json:"ModeratorComment"`
}

// rawFeed mirrors only the top-level fields ShipLog reads out of the ~24MB
// feed; encoding/json silently ignores every field it doesn't know about.
type rawFeed struct {
	AppList     []Entry           `json:"applist"`
	Blacklisted map[string]string `json:"blacklisted"`
	// Repositories is CA's list of template repositories it crawls (name →
	// {url, …}). Kept raw and decoded leniently: a shape change there must
	// cost us this one signal, never the whole feed.
	Repositories         json.RawMessage `json:"repositories"`
	LastUpdatedTimestamp int64           `json:"last_updated_timestamp"`
}

type rawLastUpdated struct {
	LastUpdatedTimestamp int64 `json:"last_updated_timestamp"`
}

// Feed is a parsed, matchable snapshot of the catalog.
type Feed struct {
	byName        map[string][]Entry
	byRepo        map[string][]Entry // normalized "owner/repo" -> entries sharing that repo (more than one CA template can wrap the same image)
	byTemplateURL map[string]Entry   // exact <TemplateURL> -> entry; CA's own canonical identity for a template
	// templateRepos is every template repository CA crawls, as the key
	// templateRepoKey derives ("github.com/owner/repo"): the feed's own
	// repositories list plus the repository behind every listed TemplateURL.
	templateRepos map[string]struct{}
	blacklisted   map[string]string // normalized "owner/repo" -> reason
	previous      *Feed             // the crawl before this one, if one was cached; nil otherwise
}

// Result is what the feed says about one container, after cross-crawl
// confirmation of any absence.
type Result struct {
	Listed     bool   // false only once confirmed absent across two consecutive crawls
	Deprecated bool   // CA's own "hidden from default search" flag; the app is still listed and updated
	Note       string // moderator comment (deprecated) or blacklist reason (not listed), for display
}

// Fetcher fetches and caches the feed under dir (typically the engine's
// DATA_DIR — small JSON files, no reason to separate it from the SQLite DB).
type Fetcher struct {
	dir    string
	client *http.Client
	// feedURL/lastUpdatedURL are injectable so tests run against an
	// httptest.Server instead of the real internet.
	feedURL        string
	lastUpdatedURL string
}

// NewFetcher builds a Fetcher caching under dir.
func NewFetcher(dir string) *Fetcher {
	return &Fetcher{
		dir:            dir,
		client:         &http.Client{Timeout: 30 * time.Second},
		feedURL:        defaultFeedURL,
		lastUpdatedURL: defaultLastUpdatedURL,
	}
}

// Load returns the current feed, refreshing the on-disk cache only when the
// upstream feed has actually changed since the last successful fetch. It
// returns a nil Feed and an error only when there is truly nothing to check
// against — no fresh fetch succeeded AND nothing is cached; callers must
// skip CA-based checks entirely for that sweep rather than guess.
func (f *Fetcher) Load(ctx context.Context) (*Feed, error) {
	curPath := filepath.Join(f.dir, cacheFile)
	prevPath := filepath.Join(f.dir, prevCacheFile)

	cached, cacheErr := os.ReadFile(curPath)
	cachedTS := int64(-1)
	if cacheErr == nil {
		var lu rawLastUpdated
		if json.Unmarshal(cached, &lu) == nil {
			cachedTS = lu.LastUpdatedTimestamp
		}
	}

	freshTS, pingErr := f.fetchTimestamp(ctx)
	if pingErr == nil && cacheErr == nil && freshTS == cachedTS {
		// Nothing changed upstream since our last fetch — cheap path, no 24MB
		// re-download just to confirm that.
		return parse(cached, prevPath), nil
	}

	fresh, fetchErr := f.fetchFull(ctx)
	if fetchErr != nil {
		if cacheErr != nil {
			return nil, fmt.Errorf("cafeed: fetch failed and nothing cached: %w", fetchErr)
		}
		return parse(cached, prevPath), nil // degrade to whatever we had
	}
	// Rotate the about-to-be-replaced "current" into "previous" BEFORE
	// overwriting it, so the next Load can cross-check an absence against a
	// genuinely earlier crawl. Write failures are best-effort: fresh is
	// already in hand for THIS call either way, only the next sweep's cache
	// benefit is at stake.
	if cacheErr == nil {
		_ = os.WriteFile(prevPath, cached, 0o644)
	}
	_ = os.WriteFile(curPath, fresh, 0o644)
	return parse(fresh, prevPath), nil
}

func (f *Fetcher) fetchTimestamp(ctx context.Context) (int64, error) {
	b, err := f.fetchBytes(ctx, f.lastUpdatedURL)
	if err != nil {
		return 0, err
	}
	var lu rawLastUpdated
	if err := json.Unmarshal(b, &lu); err != nil {
		return 0, err
	}
	return lu.LastUpdatedTimestamp, nil
}

func (f *Fetcher) fetchFull(ctx context.Context) ([]byte, error) {
	return f.fetchBytes(ctx, f.feedURL)
}

func (f *Fetcher) fetchBytes(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "shiplog")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cafeed: %s: unexpected status %d", url, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// parse builds a Feed from raw feed bytes, best-effort attaching the
// previous crawl (from prevPath) for absence cross-checking. A malformed or
// missing previous cache simply leaves Feed.previous nil — Lookup already
// treats that as "not yet confirmable", never as a false positive.
func parse(b []byte, prevPath string) *Feed {
	feed := parseOne(b)
	if feed == nil {
		return nil
	}
	if pb, err := os.ReadFile(prevPath); err == nil {
		feed.previous = parseOne(pb)
	}
	return feed
}

func parseOne(b []byte) *Feed {
	var raw rawFeed
	if json.Unmarshal(b, &raw) != nil {
		return nil
	}
	byName, byRepo, byTemplateURL := indexEntries(raw.AppList)
	blacklisted := make(map[string]string, len(raw.Blacklisted))
	for repo, reason := range raw.Blacklisted {
		blacklisted[normalizeRepo(repo)] = reason
	}
	return &Feed{
		byName: byName, byRepo: byRepo, byTemplateURL: byTemplateURL,
		templateRepos: indexTemplateRepos(raw.AppList, raw.Repositories),
		blacklisted:   blacklisted,
	}
}

// indexEntries builds the three lookup indexes Feed matches against: by
// display Name (freely user-renameable on the container side, so matched
// first but least reliable), by repository, and by exact TemplateURL (CA's
// own canonical identity for a template).
func indexEntries(entries []Entry) (byName, byRepo map[string][]Entry, byTemplateURL map[string]Entry) {
	byName = make(map[string][]Entry, len(entries))
	byRepo = make(map[string][]Entry, len(entries))
	byTemplateURL = make(map[string]Entry, len(entries))
	for _, e := range entries {
		if n := normalizeName(e.Name); n != "" {
			byName[n] = append(byName[n], e)
		}
		if r := normalizeRepo(e.Repository); r != "" {
			byRepo[r] = append(byRepo[r], e)
		}
		if e.TemplateURL != "" {
			byTemplateURL[e.TemplateURL] = e
		}
	}
	return byName, byRepo, byTemplateURL
}

// indexTemplateRepos collects every template repository the feed shows CA
// crawling: the explicit repositories list (name → {url}) AND the repository
// behind each listed TemplateURL, so a repo is recognised even if one of the
// two sources is missing or reshaped. Best-effort: anything it can't read just
// contributes nothing (which only makes the engine MORE conservative).
func indexTemplateRepos(entries []Entry, repositories json.RawMessage) map[string]struct{} {
	repos := make(map[string]struct{}, 2048)
	for _, e := range entries {
		if k, ok := templateRepoKey(e.TemplateURL); ok {
			repos[k] = struct{}{}
		}
	}
	var listed map[string]struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(repositories, &listed) == nil {
		for _, r := range listed {
			if k, ok := templateRepoKey(r.URL); ok {
				repos[k] = struct{}{}
			}
		}
	}
	return repos
}

// templateRepoKey reduces a template (or repository) URL to the lower-cased
// "host/owner/repo" of the repository it lives in, for the two hosts
// Community Applications crawls: GitHub (raw.githubusercontent.com, github.com
// blob/raw/tree URLs) and GitLab. A GitLab project path may nest groups: it
// ends at the "/-/" marker (".../group/sub/proj/-/raw/..."), or at a
// raw/blob/tree segment in older URLs, and a bare project URL (as the
// repositories list stores it) is kept whole. Branch, file path, ".git"
// suffix, query and case are all ignored. ok=false for any other host or a URL
// that names no repository — such a template is not one CA can have crawled.
func templateRepoKey(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	switch {
	case strings.HasPrefix(s, "https://"):
		s = s[len("https://"):]
	case strings.HasPrefix(s, "http://"):
		s = s[len("http://"):]
	default:
		return "", false
	}
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	host, path, _ := strings.Cut(s, "/")
	host = strings.TrimPrefix(strings.ToLower(host), "www.")
	segs := strings.FieldsFunc(strings.ToLower(path), func(r rune) bool { return r == '/' })
	switch host {
	case "raw.githubusercontent.com", "github.com":
		host = "github.com"
		segs = segs[:min(len(segs), 2)]
	case "gitlab.com":
		end := len(segs)
		if i := slices.Index(segs, "-"); i >= 0 {
			end = i
		} else if i := slices.IndexFunc(segs[min(len(segs), 2):], func(p string) bool {
			return p == "raw" || p == "blob" || p == "tree"
		}); i >= 0 {
			end = i + 2 // older URLs: .../owner/repo/raw/branch/file
		}
		segs = segs[:end]
	default:
		return "", false
	}
	if len(segs) < 2 {
		return "", false
	}
	segs[len(segs)-1] = strings.TrimSuffix(segs[len(segs)-1], ".git")
	return host + "/" + strings.Join(segs, "/"), true
}

// CrawlsTemplate reports whether Community Applications crawls the repository
// templateURL lives in — the precondition for ANY CA verdict about an
// installed app. An app absent from the feed was "removed from CA" only if CA
// once carried it; a template from a repository CA has never crawled (the
// user's own image with the user's own template, a private template repo, a
// copy pasted from a forum) is absent by nature, and CA has nothing to say
// about it. Judged on the CURRENT feed alone, so the answer cannot flap
// between crawls. false (also for a URL we cannot place on a known host)
// means "leave this app's CA state alone".
func (f *Feed) CrawlsTemplate(templateURL string) bool {
	key, ok := templateRepoKey(templateURL)
	if !ok {
		return false
	}
	_, crawled := f.templateRepos[key]
	return crawled
}

// Lookup reports what the feed says about a container. ok=false means
// inconclusive (an ambiguous name match with no repo/templateURL able to
// narrow it, an apparent absence with no previous crawl yet to confirm it
// against, or an absence for a template whose repository CA does not crawl —
// see CrawlsTemplate) — callers must leave the container's CA state alone,
// never treat ok=false as any kind of verdict.
func (f *Feed) Lookup(name, repo, templateURL string) (Result, bool) {
	n := normalizeName(name)
	matches := f.byName[n]
	var e Entry
	switch len(matches) {
	case 0:
		// No hit on the container's own display name. That name is whatever
		// the user typed into Unraid's Add Container form — freely renamed
		// (a shorter label, a multi-instance "-II" suffix) or just spelled
		// differently than the feed's canonical Name (observed live:
		// "TeamSpeak" vs the feed's "binhex-teamspeak"). Rescue via the more
		// stable repo/TemplateURL identity before concluding it's gone;
		// only when NEITHER resolves does a two-crawl absence actually mean
		// "removed from Community Applications".
		found := false
		if e, found = f.matchByIdentity(repo, templateURL); !found {
			if f.previous == nil {
				return Result{}, false // nothing to confirm an absence against yet
			}
			if len(f.previous.byName[n]) > 0 {
				return Result{}, false // present last crawl by name; today's gap unconfirmed
			}
			if _, prevFound := f.previous.matchByIdentity(repo, templateURL); prevFound {
				return Result{}, false // present last crawl by repo/URL; today's gap unconfirmed
			}
			if !f.CrawlsTemplate(templateURL) {
				// Absent from every crawl, but also from a repository CA never
				// crawled: the app was never here to be removed.
				return Result{}, false
			}
			return Result{Listed: false}, true // absent from two consecutive crawls, by every identity
		}
	case 1:
		e = matches[0]
	default:
		var found bool
		e, found = disambiguate(matches, repo, templateURL)
		if !found {
			return Result{}, false
		}
	}
	if reason, blacklisted := f.blacklisted[normalizeRepo(e.Repository)]; blacklisted {
		return Result{Listed: false, Note: reason}, true
	}
	return Result{Listed: true, Deprecated: e.Deprecated, Note: e.ModeratorComment}, true
}

// matchByIdentity finds an entry anywhere in the feed by repository or exact
// TemplateURL — CA's own more stable identity for a template — used to
// rescue a Lookup whose container display name matched no feed entry.
// Mirrors disambiguate's precedence (repository first, then template URL).
func (f *Feed) matchByIdentity(repo, templateURL string) (Entry, bool) {
	if repo != "" {
		if candidates := f.byRepo[normalizeRepo(repo)]; len(candidates) > 0 {
			return candidates[0], true
		}
	}
	if templateURL != "" {
		if e, ok := f.byTemplateURL[templateURL]; ok {
			return e, true
		}
	}
	return Entry{}, false
}

// disambiguate picks the one entry among several same-named candidates that
// actually matches this container, mirroring Community Applications' own
// join order (exec.php): repository prefix first, then the exact template
// URL ShipLog already trusts from the installed template.
func disambiguate(candidates []Entry, repo, templateURL string) (Entry, bool) {
	if repo != "" {
		nr := normalizeRepo(repo)
		for _, e := range candidates {
			if er := normalizeRepo(e.Repository); er != "" && strings.HasPrefix(nr, er) {
				return e, true
			}
		}
	}
	if templateURL != "" {
		for _, e := range candidates {
			if e.TemplateURL == templateURL {
				return e, true
			}
		}
	}
	return Entry{}, false
}

func normalizeName(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// normalizeRepo reduces repo to a bare "owner/repo" form, stripping ANY
// registry host (docker.io, ghcr.io, lscr.io, quay.io, ...), Docker Hub's
// default "library/" namespace, and a trailing ":tag" or "@digest". CA's
// Repository field is not consistently one registry — the same app is often
// installed from docker.io while CA's template references its ghcr.io mirror
// (observed live: docker.io/binhex/arch-teamspeak vs CA's own
// ghcr.io/binhex/arch-teamspeak listing) — and is not consistently bare
// either: CA sometimes bakes a tag straight into Repository (observed live:
// "ghcr.io/open-webui/open-webui:main"), which the container's own Repo
// field never carries (its tag is tracked separately). Host- and tag-blind
// comparison is what makes the two sides comparable.
func normalizeRepo(repo string) string {
	parts := strings.Split(repo, "/")
	if len(parts) > 2 && strings.Contains(parts[0], ".") {
		parts = parts[1:] // drop the registry host
	}
	if len(parts) > 1 && parts[0] == "library" {
		parts = parts[1:] // drop Docker Hub's default namespace
	}
	if last := len(parts) - 1; last >= 0 {
		if i := strings.IndexAny(parts[last], ":@"); i >= 0 {
			parts[last] = parts[last][:i] // drop a baked-in ":tag" or "@digest"
		}
	}
	return strings.Join(parts, "/")
}
