package resolver

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"sort"
	"strings"
)

// registryLogin is one username/secret pair read from Docker's config.json.
// It is deliberately unexported and has no String method: it must never reach a
// log line.
type registryLogin struct {
	user   string
	secret string
}

// dockerHubAliases are the names Docker's config.json uses for Docker Hub; they
// all resolve to the resolver's single Docker Hub registry host.
var dockerHubAliases = map[string]bool{
	"index.docker.io":      true,
	"docker.io":            true,
	"registry.docker.io":   true,
	"registry-1.docker.io": true,
}

// loadDockerConfig reads the Docker CLI config.json at path and returns the
// logins stored inline, keyed by resolver registry host. A missing, unreadable
// or malformed file yields nil — anonymous access is always the fallback, so
// callers never need an error.
func loadDockerConfig(path string) map[string]registryLogin {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return parseDockerConfig(data)
}

// parseDockerConfig extracts the usable inline logins from config.json bytes.
//
// Only secrets written INTO the file are used: Unraid's own update check reads
// the same file, and on a host where credsStore/credHelpers are set the auths
// map holds empty `{}` placeholders whose real secret lives behind an external
// helper binary. We never exec anything, so those entries (and identitytoken-only
// ones) are skipped rather than guessed at.
func parseDockerConfig(data []byte) map[string]registryLogin {
	var cfg struct {
		Auths map[string]struct {
			Auth     string `json:"auth"`
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil
	}

	// Several keys can normalise to one host (e.g. "docker.io" and
	// "https://index.docker.io/v1/"); walk them in sorted order so the winner is
	// deterministic rather than map-iteration luck.
	keys := make([]string, 0, len(cfg.Auths))
	for k := range cfg.Auths {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var out map[string]registryLogin
	for _, k := range keys {
		host := registryHostFromAuthKey(k)
		if host == "" {
			continue
		}
		if _, dup := out[host]; dup {
			continue
		}
		e := cfg.Auths[k]
		user, secret, ok := decodeBasicAuth(e.Auth)
		if !ok {
			user, secret = e.Username, e.Password
		}
		if user == "" || secret == "" {
			continue
		}
		if out == nil {
			out = map[string]registryLogin{}
		}
		out[host] = registryLogin{user: user, secret: secret}
	}
	return out
}

// decodeBasicAuth decodes the base64 "user:pass" form of an `auth` field,
// splitting on the FIRST colon (passwords may contain colons).
func decodeBasicAuth(auth string) (user, secret string, ok bool) {
	if auth == "" {
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(auth)
	if err != nil {
		return "", "", false
	}
	user, secret, found := strings.Cut(string(raw), ":")
	if !found || user == "" || secret == "" {
		return "", "", false
	}
	return user, secret, true
}

// registryHostFromAuthKey normalises a config.json auths key (a bare host, or a
// URL such as "https://index.docker.io/v1/") to the resolver's registry host
// name: scheme and path stripped, lower-cased, Docker Hub aliases collapsed.
func registryHostFromAuthKey(key string) string {
	h := strings.ToLower(strings.TrimSpace(key))
	h = strings.TrimPrefix(h, "https://")
	h = strings.TrimPrefix(h, "http://")
	if i := strings.IndexByte(h, '/'); i >= 0 {
		h = h[:i]
	}
	if dockerHubAliases[h] {
		return dockerHubRegistry
	}
	return h
}
