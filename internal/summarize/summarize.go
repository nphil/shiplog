// Package summarize turns a raw changelog into a short AI summary. Two
// interchangeable providers are supported: Ollama (native REST API) and
// llama-swap (or any OpenAI-compatible /v1/chat/completions server). Both are
// optional: when unconfigured the engine simply skips summarisation, falling
// back to the raw changelog.
package summarize

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/junkerderprovinz/shiplog/internal/model"
)

const maxRawChars = 6000

// buildPrompt renders the one summarisation prompt every provider shares.
func buildPrompt(c model.Container, fromTag, toTag, raw string) string {
	if len(raw) > maxRawChars {
		raw = raw[:maxRawChars]
	}
	return "You summarise a Docker image changelog for a homelab admin. " +
		"Image " + c.Repo + " from " + fromTag + " to " + toTag + ". " +
		`Reply ONLY with JSON: {"bullets":[3-5 short strings of what changes],` +
		`"breaking":[strings of breaking changes or required migration steps, empty if none],` +
		`"risk":"one short sentence"}. Be concise and factual. Changelog:` + "\n" + raw
}

// parseSummary decodes a model's JSON answer into an AISummary. A decode error
// or an all-empty answer returns an error so the caller can log and fall back.
func parseSummary(answer, modelName string) (*model.AISummary, error) {
	var out struct {
		Bullets  []string `json:"bullets"`
		Breaking []string `json:"breaking"`
		Risk     string   `json:"risk"`
	}
	if err := json.Unmarshal([]byte(answer), &out); err != nil {
		return nil, fmt.Errorf("not the expected JSON: %w", err)
	}
	if len(out.Bullets) == 0 && len(out.Breaking) == 0 && out.Risk == "" {
		return nil, fmt.Errorf("bullets/breaking/risk are all empty")
	}
	return &model.AISummary{
		Bullets:  out.Bullets,
		Breaking: out.Breaking,
		Risk:     out.Risk,
		Model:    modelName,
	}, nil
}

// snippet collapses whitespace and caps a string for a single-line log message.
func snippet(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 240 {
		s = s[:240] + "…"
	}
	if s == "" {
		s = "(empty)"
	}
	return s
}
