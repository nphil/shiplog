// Package autoupdate decides which container updates to apply automatically and
// runs them serially. The policy here is pure (no I/O), so it is exhaustively
// unit-tested; the schedule and executor build on top of it.
package autoupdate

import (
	"strings"

	"github.com/junkerderprovinz/shiplog/internal/model"
)

// Level is the SemVer threshold: auto-apply updates at or below it.
type Level int

const (
	LevelOff Level = iota
	LevelPatch
	LevelMinor
	LevelMajor
)

// ParseLevel maps a config string to a Level; anything unrecognised → LevelOff.
func ParseLevel(s string) Level {
	switch s {
	case "patch":
		return LevelPatch
	case "minor":
		return LevelMinor
	case "major":
		return LevelMajor
	default:
		return LevelOff
	}
}

// Policy is the global auto-update decision input.
type Policy struct {
	Level        Level    // SemVer threshold
	Digest       bool     // also auto-apply :latest / digest-only moves (level-less)
	ExcludeWords []string // block an otherwise-eligible update whose changelog text contains any of these (case-insensitive)
	// ExcludeContainers lists the containers that are never auto-updated (the
	// admin updates them by hand). Names match case-insensitively; see
	// ContainerExcluded.
	ExcludeContainers []string
}

// ParseExcludeWords splits the AUTOUPDATE_EXCLUDE_WORDS setting (comma-separated)
// into a clean word list: trimmed, empties dropped. Original casing is kept (only
// the MATCH is case-insensitive) so a blocked-update message can quote the word
// back to the admin exactly as they typed it.
func ParseExcludeWords(s string) []string {
	var words []string
	for _, w := range strings.Split(s, ",") {
		w = strings.TrimSpace(w)
		if w != "" {
			words = append(words, w)
		}
	}
	return words
}

// ParseExcludeContainers splits the AUTOUPDATE_EXCLUDE_CONTAINERS setting
// (comma-separated container names) into a clean list: trimmed, empties and
// repeats dropped (a repeat is the same name ignoring case). Original casing
// and order are kept so the run log can name the container as the admin typed
// it. Docker container names never contain a comma or a space, so a plain
// comma list is unambiguous.
func ParseExcludeContainers(s string) []string {
	var names []string
	for _, n := range strings.Split(s, ",") {
		n = strings.TrimSpace(n)
		if n != "" && !ContainerExcluded(n, names) {
			names = append(names, n)
		}
	}
	return names
}

// ContainerExcluded reports whether name is on the never-auto-update list,
// ignoring case. An empty list (the default) excludes nothing, so behaviour is
// unchanged until the admin opts a container out.
func ContainerExcluded(name string, excluded []string) bool {
	for _, e := range excluded {
		if strings.EqualFold(name, e) {
			return true
		}
	}
	return false
}

// MatchedExcludeWord reports the FIRST configured word found in the pending
// update's changelog text (case-insensitive substring match against cl.Raw —
// the body of the target release), or "" when nothing matches. Nil-safe: no
// changelog, an empty body, or no configured words all report no match, so this
// is a pure opt-in safety net that never blocks when there is nothing to check
// against. A container without release notes (e.g. the version-delta Fallback,
// which carries an empty Raw) is therefore never blocked by this check — only a
// changelog whose own text names the danger word is.
func MatchedExcludeWord(cl *model.Changelog, words []string) string {
	if cl == nil || cl.Raw == "" || len(words) == 0 {
		return ""
	}
	raw := strings.ToLower(cl.Raw)
	for _, w := range words {
		if strings.Contains(raw, strings.ToLower(w)) {
			return w
		}
	}
	return ""
}

// Eligible reports whether the container's available update should be applied
// under the policy. Unknown (non-SemVer) bumps are never eligible; digest moves
// only via the separate toggle; SemVer bumps only at or below the threshold.
func Eligible(st model.UpdateStatus, p Policy) bool {
	if !st.HasUpdate() {
		return false
	}
	switch st.Kind {
	case model.KindPatch:
		return p.Level >= LevelPatch
	case model.KindMinor:
		return p.Level >= LevelMinor
	case model.KindMajor:
		return p.Level >= LevelMajor
	case model.KindDigest:
		return p.Digest
	default: // KindUnknown, KindNone, anything unclassified — never auto
		return false
	}
}
