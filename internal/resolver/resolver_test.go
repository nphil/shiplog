package resolver

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestServer builds a fake OCI registry v2 backend. When requireAuth is
// true the registry rejects unauthenticated requests with a 401 carrying a
// WWW-Authenticate header that points the resolver at this server's /token
// endpoint; the second (bearer-authenticated) attempt then succeeds.
func newTestServer(requireAuth bool) *httptest.Server {
	mux := http.NewServeMux()

	// Anonymous bearer token endpoint.
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"x"}`))
	})

	// Tags list.
	mux.HandleFunc("/v2/lib/app/tags/list", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"lib/app","tags":["1.0.0","1.2.0","1.1.0","latest","nightly"]}`))
	})

	// Manifest HEAD for tag 1.0.0 → digest sha256:NEW.
	mux.HandleFunc("/v2/lib/app/manifests/1.0.0", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Docker-Content-Digest", "sha256:NEW")
		w.WriteHeader(http.StatusOK)
	})

	// Manifest HEAD for the newest semver tag 1.2.0 → digest sha256:NEWEST.
	mux.HandleFunc("/v2/lib/app/manifests/1.2.0", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Docker-Content-Digest", "sha256:NEWEST")
		w.WriteHeader(http.StatusOK)
	})

	// Manifest HEAD for tag latest → digest sha256:LATEST.
	mux.HandleFunc("/v2/lib/app/manifests/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Docker-Content-Digest", "sha256:LATEST")
		w.WriteHeader(http.StatusOK)
	})

	if !requireAuth {
		return httptest.NewServer(mux)
	}

	// Wrap the mux: every /v2/ request must carry a bearer token, otherwise
	// reply 401 + WWW-Authenticate. /token and /v2/ (version check) stay open.
	srv := httptest.NewServer(nil)
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v2/lib/app/") && r.Header.Get("Authorization") == "" {
			realm := srv.URL + "/token"
			w.Header().Set("WWW-Authenticate",
				`Bearer realm="`+realm+`",service="registry.test",scope="repository:lib/app:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})
	return srv
}

// newResolverFor returns a Resolver whose baseURL maps every host to the test
// server, so the resolver talks only to the fake registry. Backoff sleeps and
// the per-host gate are neutered so tests run instantly and deterministically.
func newResolverFor(srv *httptest.Server) *Resolver {
	r := New()
	r.baseURL = func(host string) string { return srv.URL }
	r.gate = newHostGate(0)                           // no inter-request spacing in tests
	r.sleep = func(context.Context, time.Duration) {} // don't actually back off
	return r
}

func TestResolve_NewestTagAndDigest(t *testing.T) {
	srv := newTestServer(false)
	defer srv.Close()

	r := newResolverFor(srv)

	newestTag, sameTagDigest, newestVerTag, newestVerDigest, err := r.Resolve(
		context.Background(), "docker.io/lib/app", "1.0.0", "sha256:OLD")
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if newestTag != "1.2.0" {
		t.Errorf("newestTag = %q, want %q", newestTag, "1.2.0")
	}
	if sameTagDigest != "sha256:NEW" {
		t.Errorf("sameTagDigest = %q, want %q", sameTagDigest, "sha256:NEW")
	}
	if newestVerTag != "1.2.0" {
		t.Errorf("newestVerTag = %q, want %q", newestVerTag, "1.2.0")
	}
	// Pinned semver tag: the proof-digest fetch is skipped, so it stays empty.
	if newestVerDigest != "" {
		t.Errorf("newestVerDigest = %q, want empty for a pinned tag", newestVerDigest)
	}
}

func TestResolve_AnonymousTokenFlow(t *testing.T) {
	srv := newTestServer(true) // first call 401, must follow WWW-Authenticate
	defer srv.Close()

	r := newResolverFor(srv)

	newestTag, sameTagDigest, _, _, err := r.Resolve(
		context.Background(), "docker.io/lib/app", "1.0.0", "sha256:OLD")
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if newestTag != "1.2.0" {
		t.Errorf("newestTag = %q, want %q", newestTag, "1.2.0")
	}
	if sameTagDigest != "sha256:NEW" {
		t.Errorf("sameTagDigest = %q, want %q", sameTagDigest, "sha256:NEW")
	}
}

func TestResolve_CachesBearerTokenAcrossRequests(t *testing.T) {
	var tokenHits atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/lib/app/tags/list", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tags":["1.0.0","1.2.0"]}`))
	})
	mux.HandleFunc("/v2/lib/app/manifests/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Docker-Content-Digest", "sha256:NEW")
	})
	srv := httptest.NewServer(nil)
	defer srv.Close()
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			tokenHits.Add(1)
			_, _ = w.Write([]byte(`{"token":"x","expires_in":300}`))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v2/") && r.Header.Get("Authorization") != "Bearer x" {
			w.Header().Set("WWW-Authenticate",
				`Bearer realm="`+srv.URL+`/token",service="registry.test"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})

	r := newResolverFor(srv)
	for i := 0; i < 2; i++ {
		if _, _, _, _, err := r.Resolve(context.Background(), "docker.io/lib/app", "1.0.0", ""); err != nil {
			t.Fatalf("Resolve %d returned error: %v", i, err)
		}
	}
	// One 401 fills the cache; every later request (the manifest HEAD of the
	// first Resolve AND the entire second Resolve) rides the cached token.
	if got := tokenHits.Load(); got != 1 {
		t.Errorf("token endpoint hit %d times, want exactly 1 (cache must cover follow-up requests)", got)
	}
}

func TestResolve_ExpiredCachedTokenHealsViaChallenge(t *testing.T) {
	var tokenHits atomic.Int64
	srv := httptest.NewServer(nil)
	defer srv.Close()
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			tokenHits.Add(1)
			_, _ = w.Write([]byte(`{"token":"x"}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer x" {
			w.Header().Set("WWW-Authenticate",
				`Bearer realm="`+srv.URL+`/token",service="registry.test"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/tags/list"):
			_, _ = w.Write([]byte(`{"tags":["1.0.0"]}`))
		default:
			w.Header().Set("Docker-Content-Digest", "sha256:NEW")
		}
	})

	r := newResolverFor(srv)
	if _, _, _, _, err := r.Resolve(context.Background(), "docker.io/lib/app", "1.0.0", ""); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	// Let the cached token expire; the next Resolve must transparently
	// re-authenticate instead of failing.
	r.now = func() time.Time { return time.Now().Add(time.Hour) }
	if _, _, _, _, err := r.Resolve(context.Background(), "docker.io/lib/app", "1.0.0", ""); err != nil {
		t.Fatalf("second Resolve after token expiry: %v", err)
	}
	if got := tokenHits.Load(); got != 2 {
		t.Errorf("token endpoint hit %d times, want 2 (one per expiry window)", got)
	}
}

func TestResolve_BreakerTripsOnTokenRealm429(t *testing.T) {
	// The most common real-world 429 source is the TOKEN endpoint
	// (auth.docker.io / ghcr token issuance), not the /v2/ endpoints. A hard
	// token 429 must mute the host too, or the sweep keeps re-hammering it.
	var tokenHits atomic.Int64
	srv := httptest.NewServer(nil)
	defer srv.Close()
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			tokenHits.Add(1)
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("WWW-Authenticate",
			`Bearer realm="`+srv.URL+`/token",service="registry.test"`)
		w.WriteHeader(http.StatusUnauthorized)
	})

	r := newResolverFor(srv)
	if _, _, _, _, err := r.Resolve(context.Background(), "docker.io/lib/app", "1.0.0", ""); err == nil {
		t.Fatal("first Resolve must fail when the token realm hard-429s")
	}
	hitsAfterFirst := tokenHits.Load()

	_, _, _, _, err := r.Resolve(context.Background(), "docker.io/lib/app", "1.0.0", "")
	if err == nil || !strings.Contains(err.Error(), "backing off") {
		t.Fatalf("second Resolve error = %v, want a backing-off error", err)
	}
	if got := tokenHits.Load(); got != hitsAfterFirst {
		t.Errorf("muted host's token realm still received %d extra requests", got-hitsAfterFirst)
	}
}

func TestResolve_BreakerMutesHostAfterHard429(t *testing.T) {
	var v2Hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v2/") {
			v2Hits.Add(1)
		}
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	r := newResolverFor(srv)
	if _, _, _, _, err := r.Resolve(context.Background(), "docker.io/lib/app", "1.0.0", ""); err == nil {
		t.Fatal("first Resolve must fail on a hard 429")
	}
	hitsAfterFirst := v2Hits.Load()

	// Host is now muted: the next Resolve must answer without any network and
	// say why.
	_, _, _, _, err := r.Resolve(context.Background(), "docker.io/lib/app", "1.0.0", "")
	if err == nil || !strings.Contains(err.Error(), "backing off") {
		t.Fatalf("second Resolve error = %v, want a backing-off error", err)
	}
	if got := v2Hits.Load(); got != hitsAfterFirst {
		t.Errorf("muted host still received %d extra requests", got-hitsAfterFirst)
	}

	// After the window has passed, the host is queried again.
	r.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	_, _, _, _, _ = r.Resolve(context.Background(), "docker.io/lib/app", "1.0.0", "")
	if got := v2Hits.Load(); got == hitsAfterFirst {
		t.Error("host was not retried after the breaker window elapsed")
	}
}

func TestResolve_NonSemverTag(t *testing.T) {
	srv := newTestServer(false)
	defer srv.Close()

	r := newResolverFor(srv)

	newestTag, sameTagDigest, newestVerTag, newestVerDigest, err := r.Resolve(
		context.Background(), "docker.io/lib/app", "latest", "sha256:OLD")
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if newestTag != "latest" {
		t.Errorf("newestTag = %q, want %q", newestTag, "latest")
	}
	if sameTagDigest != "sha256:LATEST" {
		t.Errorf("sameTagDigest = %q, want %q", sameTagDigest, "sha256:LATEST")
	}
	// A rolling tag still resolves the newest version AND its digest, so the
	// engine can prove which version a :latest container is running.
	if newestVerTag != "1.2.0" {
		t.Errorf("newestVerTag = %q, want %q", newestVerTag, "1.2.0")
	}
	if newestVerDigest != "sha256:NEWEST" {
		t.Errorf("newestVerDigest = %q, want %q", newestVerDigest, "sha256:NEWEST")
	}
}

func TestResolve_FollowsTagPagination(t *testing.T) {
	// Page 1 returns old tags + a Link to page 2; page 2 has the newer tags.
	// Reading only page 1 would pick 0.59; following the Link finds 1.8.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/lib/app/tags/list" && r.URL.Query().Get("last") == "":
			w.Header().Set("Link", `</v2/lib/app/tags/list?n=500&last=0.59>; rel="next"`)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"tags":["0.58","0.59"]}`))
		case r.URL.Path == "/v2/lib/app/tags/list":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"tags":["1.7","1.8"]}`)) // page 2, no Link → stop
		case strings.HasPrefix(r.URL.Path, "/v2/lib/app/manifests/"):
			w.Header().Set("Docker-Content-Digest", "sha256:X")
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	r := newResolverFor(srv)
	newest, _, _, _, err := r.Resolve(context.Background(), "docker.io/lib/app", "1.7", "sha256:o")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if newest != "1.8" {
		t.Errorf("newestTag = %q, want 1.8 (must follow pagination to page 2)", newest)
	}
}

// When Docker Hub credentials are configured, the token request for a docker.io
// image must carry HTTP Basic auth (so the issued token has the authenticated,
// higher rate limit). For other registries, no credentials must leak.
func TestResolve_DockerHubBasicAuthOnToken(t *testing.T) {
	var gotTokenAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		gotTokenAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"x"}`))
	})
	mux.HandleFunc("/v2/library/app/tags/list", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tags":["1.0.0","1.2.0","latest"]}`))
	})
	mux.HandleFunc("/v2/library/app/manifests/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Docker-Content-Digest", "sha256:Z")
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(nil)
	defer srv.Close()
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v2/library/app/") && r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate",
				`Bearer realm="`+srv.URL+`/token",service="registry.docker.io",scope="repository:library/app:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})

	r := newResolverFor(srv).WithDockerHubAuth("alice", "dh-secret")
	// "docker.io/library/app" → host registry-1.docker.io → Basic auth expected.
	if _, _, _, _, err := r.Resolve(context.Background(), "docker.io/library/app", "latest", ""); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:dh-secret"))
	if gotTokenAuth != want {
		t.Errorf("token request Authorization = %q, want %q", gotTokenAuth, want)
	}
}

func TestNewestSemver_AcceptsTwoPartTags(t *testing.T) {
	cases := []struct {
		tags []string
		want string
	}{
		{[]string{"0.16", "0.17", "0.18", "latest"}, "0.18"},                // OpenHands-style two-part tags
		{[]string{"0.17.5", "0.18", "0.16.9"}, "0.18"},                      // a two-part tag beats an older three-part one
		{[]string{"1.0.0", "1.2.0", "1.1.0", "latest", "nightly"}, "1.2.0"}, // three-part still works
		{[]string{"1.8.0-rc1", "1.7.9"}, "1.7.9"},                           // prerelease never ranks as newest
		{[]string{"latest", "nightly", "main"}, ""},                         // nothing semver
	}
	for _, c := range cases {
		if got := newestSemver(c.tags); got != c.want {
			t.Errorf("newestSemver(%v) = %q, want %q", c.tags, got, c.want)
		}
	}
}

// A transient 429 on tags/list must be retried (honouring Retry-After) and
// succeed once the registry stops rate-limiting.
func TestResolve_RetriesOn429ThenSucceeds(t *testing.T) {
	var tagsCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/lib/app/tags/list", func(w http.ResponseWriter, r *http.Request) {
		// First two attempts: 429 with a Retry-After; third: the real list.
		if atomic.AddInt32(&tagsCalls, 1) <= 2 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tags":["1.0.0","1.2.0","latest"]}`))
	})
	mux.HandleFunc("/v2/lib/app/manifests/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Docker-Content-Digest", "sha256:OK")
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r := newResolverFor(srv)
	newest, _, _, _, err := r.Resolve(context.Background(), "docker.io/lib/app", "1.0.0", "")
	if err != nil {
		t.Fatalf("Resolve after retried 429: %v", err)
	}
	if newest != "1.2.0" {
		t.Errorf("newestTag = %q, want 1.2.0", newest)
	}
	if got := atomic.LoadInt32(&tagsCalls); got != 3 {
		t.Errorf("tags/list called %d×, want 3 (two 429s + one success)", got)
	}
}

// A 429 that never clears (exceeds the retry budget) surfaces a clear, honest
// rate-limit error rather than a generic "unexpected status".
func TestResolve_429ExhaustsRetriesWithClearError(t *testing.T) {
	var tagsCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/tags/list") {
			atomic.AddInt32(&tagsCalls, 1)
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	r := newResolverFor(srv)
	_, _, _, _, err := r.Resolve(context.Background(), "docker.io/lib/app", "1.0.0", "")
	if err == nil {
		t.Fatal("expected an error when 429 persists")
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("error = %q, want a clear rate-limit message", err.Error())
	}
	// 1 initial + maxRetries attempts.
	if got := atomic.LoadInt32(&tagsCalls); got != int32(maxRetries+1) {
		t.Errorf("tags/list called %d×, want %d", got, maxRetries+1)
	}
}

// When a GitHub token is configured, the token request for ghcr.io / lscr.io
// must carry HTTP Basic auth (any user, token as password) so the issued bearer
// has the authenticated rate limit. The token never leaks to other registries.
func TestResolve_GitHubTokenBasicAuthOnGHCR(t *testing.T) {
	const tok = "ghp_secret"
	for _, host := range []string{"ghcr.io", "lscr.io"} {
		t.Run(host, func(t *testing.T) {
			var gotTokenAuth string
			mux := http.NewServeMux()
			mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
				gotTokenAuth = r.Header.Get("Authorization")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"token":"x"}`))
			})
			mux.HandleFunc("/v2/o/app/tags/list", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"tags":["1.0.0","1.2.0","latest"]}`))
			})
			mux.HandleFunc("/v2/o/app/manifests/", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Docker-Content-Digest", "sha256:Z")
				w.WriteHeader(http.StatusOK)
			})
			srv := httptest.NewServer(nil)
			defer srv.Close()
			srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/v2/o/app/") && r.Header.Get("Authorization") == "" {
					w.Header().Set("WWW-Authenticate",
						`Bearer realm="`+srv.URL+`/token",service="`+host+`",scope="repository:o/app:pull"`)
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				mux.ServeHTTP(w, r)
			})

			r := newResolverFor(srv).WithGitHubToken(tok)
			if _, _, _, _, err := r.Resolve(context.Background(), host+"/o/app", "latest", ""); err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x:"+tok))
			if gotTokenAuth != want {
				t.Errorf("token request Authorization = %q, want %q", gotTokenAuth, want)
			}
		})
	}
}

// A GitHub token must NEVER be attached to a non-ghcr registry's token request.
func TestResolve_GitHubTokenNotLeakedToOtherRegistries(t *testing.T) {
	var gotTokenAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		gotTokenAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"x"}`))
	})
	mux.HandleFunc("/v2/o/app/tags/list", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tags":["1.0.0","latest"]}`))
	})
	mux.HandleFunc("/v2/o/app/manifests/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Docker-Content-Digest", "sha256:Z")
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(nil)
	defer srv.Close()
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v2/o/app/") && r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate",
				`Bearer realm="`+srv.URL+`/token",service="quay.io",scope="repository:o/app:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})

	r := newResolverFor(srv).WithGitHubToken("ghp_secret")
	if _, _, _, _, err := r.Resolve(context.Background(), "quay.io/o/app", "latest", ""); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if gotTokenAuth != "" {
		t.Errorf("token request to quay.io carried Authorization %q, want none", gotTokenAuth)
	}
}

// tokenRegistry is a fake registry that demands a bearer token for o/app and
// records the Authorization header of every token request and of every /v2/
// request, so tests can prove where a credential did and did not travel.
type tokenRegistry struct {
	*httptest.Server
	mu        sync.Mutex
	tokenAuth []string
	v2Auth    []string
}

// newTokenRegistry starts the fake registry. The Bearer challenge points at
// realmBase+"/token"; an empty realmBase means the registry's own URL.
func newTokenRegistry(t *testing.T, realmBase string) *tokenRegistry {
	t.Helper()
	reg := &tokenRegistry{}
	reg.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			reg.mu.Lock()
			reg.tokenAuth = append(reg.tokenAuth, r.Header.Get("Authorization"))
			reg.mu.Unlock()
			_, _ = w.Write([]byte(`{"token":"tok"}`))
		case strings.HasPrefix(r.URL.Path, "/v2/o/app/"):
			reg.mu.Lock()
			reg.v2Auth = append(reg.v2Auth, r.Header.Get("Authorization"))
			reg.mu.Unlock()
			if r.Header.Get("Authorization") != "Bearer tok" {
				base := realmBase
				if base == "" {
					base = reg.URL
				}
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+base+`/token",service="reg",scope="repository:o/app:pull"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if strings.HasSuffix(r.URL.Path, "/tags/list") {
				_, _ = w.Write([]byte(`{"tags":["1.0.0","latest"]}`))
				return
			}
			w.Header().Set("Docker-Content-Digest", "sha256:Z")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(reg.Close)
	return reg
}

func (reg *tokenRegistry) tokenAuths() []string {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	return append([]string(nil), reg.tokenAuth...)
}

// noBasicOnV2 fails the test if any /v2/ request ever carried Basic credentials:
// stored logins belong on the token request only.
func (reg *tokenRegistry) noBasicOnV2(t *testing.T) {
	t.Helper()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	for _, a := range reg.v2Auth {
		if strings.HasPrefix(a, "Basic ") {
			t.Errorf("/v2/ request carried Basic credentials")
		}
	}
}

func basicHeader(userPass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(userPass))
}

// A docker-config login for a self-hosted registry is sent as Basic on that
// registry's own token request (and nowhere else).
func TestResolve_DockerConfigLoginBasicOnOwnRealm(t *testing.T) {
	reg := newTokenRegistry(t, "")
	cfg := writeDockerConfig(t, `{"auths":{"git.example.org":{"auth":"`+
		base64.StdEncoding.EncodeToString([]byte("alice:s3cret"))+`"}}}`)

	r := newResolverFor(reg.Server).WithDockerConfig(cfg)
	if _, _, _, _, err := r.Resolve(context.Background(), "git.example.org/o/app", "latest", ""); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got := reg.tokenAuths()
	if len(got) == 0 || got[0] != basicHeader("alice:s3cret") {
		t.Errorf("token request Authorization = %q, want %q", got, basicHeader("alice:s3cret"))
	}
	reg.noBasicOnV2(t)
}

// A login stored for host A must not be sent when resolving host B.
func TestResolve_DockerConfigLoginNotSentToOtherHost(t *testing.T) {
	reg := newTokenRegistry(t, "")
	cfg := writeDockerConfig(t, `{"auths":{"a.example.org":{"username":"alice","password":"s3cret"}}}`)

	r := newResolverFor(reg.Server).WithDockerConfig(cfg)
	if _, _, _, _, err := r.Resolve(context.Background(), "b.example.org/o/app", "latest", ""); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for _, a := range reg.tokenAuths() {
		if a != "" {
			t.Errorf("token request for b.example.org carried Authorization (a.example.org's login leaked)")
		}
	}
}

// A login must not be sent when the challenge points the token request at a
// host other than the registry itself, even for the login's own registry.
func TestResolve_DockerConfigLoginNotSentToForeignRealm(t *testing.T) {
	foreign := newTokenRegistry(t, "")
	reg := newTokenRegistry(t, foreign.URL)
	cfg := writeDockerConfig(t, `{"auths":{"git.example.org":{"username":"alice","password":"s3cret"}}}`)

	r := newResolverFor(reg.Server).WithDockerConfig(cfg)
	if _, _, _, _, err := r.Resolve(context.Background(), "git.example.org/o/app", "latest", ""); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got := foreign.tokenAuths()
	if len(got) == 0 {
		t.Fatal("foreign realm was never contacted; test does not exercise the realm check")
	}
	for _, a := range got {
		if a != "" {
			t.Errorf("foreign realm received Authorization (login leaked off-host)")
		}
	}
}

// A stored login the token realm now rejects (expired PAT, rotated password)
// must not make a PUBLIC image unreadable — an anonymous request never needed
// it. The resolver retries the token request once without the login: a public
// image resolves, a private one keeps failing with an ordinary error that can
// never read as "image removed".
func TestResolve_StaleDockerConfigLoginFallsBackToAnonymous(t *testing.T) {
	for _, public := range []bool{true, false} {
		var mu sync.Mutex
		var tokenAuth []string
		srv := httptest.NewServer(nil)
		srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/token":
				mu.Lock()
				tokenAuth = append(tokenAuth, r.Header.Get("Authorization"))
				mu.Unlock()
				if r.Header.Get("Authorization") != "" || !public {
					w.WriteHeader(http.StatusUnauthorized) // stale login rejected; a private image offers no anonymous token
					return
				}
				_, _ = w.Write([]byte(`{"token":"tok"}`))
			case r.Header.Get("Authorization") != "Bearer tok":
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token",service="reg"`)
				w.WriteHeader(http.StatusUnauthorized)
			case strings.HasSuffix(r.URL.Path, "/tags/list"):
				_, _ = w.Write([]byte(`{"tags":["1.0.0","latest"]}`))
			default:
				w.Header().Set("Docker-Content-Digest", "sha256:Z")
			}
		})
		cfg := writeDockerConfig(t, `{"auths":{"git.example.org":{"username":"alice","password":"expired"}}}`)

		r := newResolverFor(srv).WithDockerConfig(cfg)
		_, digest, _, _, err := r.Resolve(context.Background(), "git.example.org/o/app", "latest", "")
		srv.Close()

		mu.Lock()
		got := append([]string(nil), tokenAuth...)
		mu.Unlock()
		if len(got) != 2 || got[0] != basicHeader("alice:expired") || got[1] != "" {
			t.Errorf("public=%v: token requests = %q, want the stored login first, then one anonymous retry", public, got)
		}
		if public {
			if err != nil || digest != "sha256:Z" {
				t.Errorf("a public image must still resolve past a stale login, got digest=%q err=%v", digest, err)
			}
			continue
		}
		if err == nil {
			t.Error("a private image with a stale login must fail")
		} else if errors.Is(err, ErrRepoNotFound) {
			t.Errorf("an unauthorized answer must never read as repository-not-found: %v", err)
		}
	}
}

// A realm that answers the credentialed token request with a redirect must not
// get the login carried along: Go forwards Authorization to the same host (even
// across an https→http downgrade) and to subdomains, which would put a stored
// registry login on the wire in the clear or hand it to another name. The
// redirect is not followed — the 3xx surfaces as an ordinary lookup error — so
// the redirect target is never contacted at all. (Two loopback servers share a
// host name, which is exactly the case Go would forward Authorization for.)
func TestResolve_CredentialedTokenRequestNeverFollowsRedirect(t *testing.T) {
	cases := []struct {
		name  string
		image string
		set   func(r *Resolver, cfg string) *Resolver
	}{
		{"docker-config login", "git.example.org/o/app", func(r *Resolver, cfg string) *Resolver { return r.WithDockerConfig(cfg) }},
		{"explicit GitHub token", "ghcr.io/o/app", func(r *Resolver, _ string) *Resolver { return r.WithGitHubToken("ghp_secret") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			target := newTokenRegistry(t, "")
			registry := httptest.NewServer(nil)
			defer registry.Close()
			registry.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					http.Redirect(w, r, target.URL+"/token", http.StatusFound)
					return
				}
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+registry.URL+`/token",service="reg"`)
				w.WriteHeader(http.StatusUnauthorized)
			})
			cfg := writeDockerConfig(t, `{"auths":{"git.example.org":{"username":"alice","password":"s3cret"}}}`)

			r := c.set(newResolverFor(registry), cfg)
			_, _, _, _, err := r.Resolve(context.Background(), c.image, "latest", "")
			if err == nil {
				t.Fatal("a redirecting token realm must fail the lookup, not succeed through the redirect")
			}
			if errors.Is(err, ErrRepoNotFound) {
				t.Errorf("a redirect must never read as repository-not-found: %v", err)
			}
			if got := target.tokenAuths(); len(got) != 0 {
				t.Errorf("the redirect target was contacted with Authorization %q; the login must never follow a redirect", got)
			}
		})
	}
}

// Precedence on ghcr.io: an explicit GITHUB_TOKEN beats the docker-config login,
// which in turn is used when no explicit token is set.
func TestResolve_DockerConfigLoginPrecedenceOnGHCR(t *testing.T) {
	cfgJSON := `{"auths":{"ghcr.io":{"username":"dockeruser","password":"dockerpass"}}}`
	cases := []struct {
		name    string
		ghToken string
		want    string
	}{
		{"explicit GitHub token wins", "ghp_secret", basicHeader("x:ghp_secret")},
		{"docker-config login when no token", "", basicHeader("dockeruser:dockerpass")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := newTokenRegistry(t, "")
			r := newResolverFor(reg.Server).WithDockerConfig(writeDockerConfig(t, cfgJSON)).WithGitHubToken(tc.ghToken)
			if _, _, _, _, err := r.Resolve(context.Background(), "ghcr.io/o/app", "latest", ""); err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			got := reg.tokenAuths()
			if len(got) == 0 || got[0] != tc.want {
				t.Errorf("token request Authorization = %q, want %q", got, tc.want)
			}
		})
	}
}

// The Docker Hub alias key https://index.docker.io/v1/ supplies the login for
// docker.io images; explicit DOCKERHUB_* credentials still take precedence.
func TestResolve_DockerConfigDockerHubAlias(t *testing.T) {
	cfgJSON := `{"auths":{"https://index.docker.io/v1/":{"username":"hubuser","password":"hubpass"}}}`
	cases := []struct {
		name string
		hub  [2]string
		want string
	}{
		{"docker-config alias", [2]string{}, basicHeader("hubuser:hubpass")},
		{"explicit Docker Hub creds win", [2]string{"envuser", "envtoken"}, basicHeader("envuser:envtoken")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := newTokenRegistry(t, "")
			r := newResolverFor(reg.Server).WithDockerConfig(writeDockerConfig(t, cfgJSON)).
				WithDockerHubAuth(tc.hub[0], tc.hub[1])
			if _, _, _, _, err := r.Resolve(context.Background(), "docker.io/o/app", "latest", ""); err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			got := reg.tokenAuths()
			if len(got) == 0 || got[0] != tc.want {
				t.Errorf("token request Authorization = %q, want %q", got, tc.want)
			}
		})
	}
}

// ErrRepoNotFound must only ever come from a definitive NAME_UNKNOWN 404 on the
// tags list. Private/denied/inconclusive answers must read as ordinary errors so
// the engine never flags a live image as "removed from the registry".
func TestResolve_ErrRepoNotFoundOnlyForNameUnknown(t *testing.T) {
	const challenge = `Bearer realm="%s/token",service="reg",scope="repository:o/app:pull"`
	cases := []struct {
		name       string
		challenge  bool // tags/list answers 401 + challenge until a bearer is presented
		tokenCode  int
		tagsCode   int
		tagsBody   string
		wantAbsent bool
	}{
		{"404 NAME_UNKNOWN", false, 200, 404, `{"errors":[{"code":"NAME_UNKNOWN","message":"repository name not known to registry"}]}`, true},
		{"404 NAME_UNKNOWN empty message", false, 200, 404, `{"errors":[{"code":"NAME_UNKNOWN","message":""}]}`, true},
		{"404 NAME_UNKNOWN after token exchange", true, 200, 404, `{"errors":[{"code":"NAME_UNKNOWN"}]}`, true},
		{"bare 404", false, 200, 404, ``, false},
		{"404 html", false, 200, 404, `<html>not found</html>`, false},
		{"404 other code", false, 200, 404, `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`, false},
		{"401 challenge then token realm 401", true, 401, 0, ``, false},
		{"401 challenge then token realm 403", true, 403, 0, ``, false},
		{"token realm 404", true, 404, 0, ``, false},
		{"403 DENIED on tags/list", true, 200, 403, `{"errors":[{"code":"DENIED","message":"requested access to the resource is denied"}]}`, false},
		{"500 on tags/list", false, 200, 500, ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(nil)
			srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					w.WriteHeader(tc.tokenCode)
					if tc.tokenCode == 200 {
						_, _ = w.Write([]byte(`{"token":"tok"}`))
					}
					return
				}
				if tc.challenge && r.Header.Get("Authorization") == "" {
					w.Header().Set("WWW-Authenticate", strings.Replace(challenge, "%s", srv.URL, 1))
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				w.WriteHeader(tc.tagsCode)
				_, _ = w.Write([]byte(tc.tagsBody))
			})
			defer srv.Close()

			_, _, _, _, err := newResolverFor(srv).Resolve(context.Background(), "quay.io/o/app", "latest", "")
			if err == nil {
				t.Fatal("Resolve succeeded, want an error")
			}
			if got := errors.Is(err, ErrRepoNotFound); got != tc.wantAbsent {
				t.Errorf("errors.Is(err, ErrRepoNotFound) = %v, want %v (err: %v)", got, tc.wantAbsent, err)
			}
		})
	}
}

func TestRetryAfter(t *testing.T) {
	const def = 2 * time.Second
	cases := []struct {
		name string
		hdr  string
		want time.Duration
	}{
		{"empty falls back to default", "", def},
		{"delta-seconds", "5", 5 * time.Second},
		{"zero seconds", "0", 0},
		{"clamped to max", "9999", maxRetryWait},
		{"garbage falls back to default", "soon", def},
	}
	for _, c := range cases {
		if got := retryAfter(c.hdr, def); got != c.want {
			t.Errorf("%s: retryAfter(%q) = %v, want %v", c.name, c.hdr, got, c.want)
		}
	}
}

func TestBackoffGrowsAndCaps(t *testing.T) {
	// Each attempt's backoff (minus jitter) must be >= the base*2^attempt, and
	// never exceed maxBackoff + one jitter quantum.
	prev := time.Duration(0)
	for attempt := 0; attempt < 8; attempt++ {
		d := backoff(attempt)
		if d < baseBackoff {
			t.Errorf("attempt %d: backoff %v < base %v", attempt, d, baseBackoff)
		}
		if d > maxBackoff+baseBackoff {
			t.Errorf("attempt %d: backoff %v exceeds cap %v", attempt, d, maxBackoff+baseBackoff)
		}
		if attempt > 0 && attempt < 5 && d < prev {
			t.Errorf("attempt %d: backoff %v not growing (prev %v)", attempt, d, prev)
		}
		prev = d
	}
}

func TestIsTransient(t *testing.T) {
	cases := map[int]bool{
		http.StatusTooManyRequests:    true,
		http.StatusServiceUnavailable: true,
		http.StatusOK:                 false,
		http.StatusNotFound:           false,
		http.StatusForbidden:          false,
	}
	for code, want := range cases {
		if got := isTransient(code); got != want {
			t.Errorf("isTransient(%d) = %v, want %v", code, got, want)
		}
	}
}

func TestSplitRepo(t *testing.T) {
	cases := []struct {
		repo     string
		wantHost string
		wantPath string
	}{
		{"docker.io/library/redis", "registry-1.docker.io", "library/redis"},
		{"docker.io/lib/app", "registry-1.docker.io", "lib/app"},
		{"ghcr.io/x/y", "ghcr.io", "x/y"},
		{"myreg.example.com/ns/name", "myreg.example.com", "ns/name"},
	}
	for _, c := range cases {
		host, path := splitRepo(c.repo)
		if host != c.wantHost || path != c.wantPath {
			t.Errorf("splitRepo(%q) = (%q, %q), want (%q, %q)",
				c.repo, host, path, c.wantHost, c.wantPath)
		}
	}
}
