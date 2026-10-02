package autoupdate

import (
	"context"

	"github.com/junkerderprovinz/shiplog/internal/model"
	"github.com/junkerderprovinz/shiplog/internal/updater"
)

// Lister supplies the current per-container update statuses (the store).
type Lister interface {
	List() ([]model.UpdateStatus, error)
}

// Outcome is the per-container result of one auto-update run.
type Outcome struct {
	Name    string
	From    string
	To      string
	Level   string
	Updated bool
	Err     error
	// Blocked is set when the update was level-eligible but the changelog matched
	// a configured exclude word (BlockedWord). Updated is false and Err is nil in
	// that case — it is a deliberate skip, not a failure. Set in both real and
	// dry-run mode, so the safety switch is visible ("would have been blocked")
	// before anyone has to trust it in production.
	Blocked     bool
	BlockedWord string
	// Skipped is set when the update was level-eligible but the container is on
	// the admin's exclude-containers list (Policy.ExcludeContainers): they update
	// it by hand. Like Blocked it is a deliberate skip — Updated false, Err nil,
	// no Updater call — and is set in real and dry-run mode alike, so the
	// exclusion is visible in a dry run. It is the admin's own choice rather than
	// the changelog safety net, so it is reported separately from Blocked.
	Skipped bool
}

// Result aggregates one auto-update run.
type Result struct {
	Outcomes []Outcome
	DryRun   bool
}

// Notable reports whether the run is worth pushing to the notification
// channels. A run whose only outcomes are containers the admin excluded on
// purpose is not: they are already told, in the run log, that those containers
// are skipped, and a daily ping about a container they update by hand is noise.
func (r Result) Notable() bool {
	for _, o := range r.Outcomes {
		if !o.Skipped {
			return true
		}
	}
	return false
}

// Executor applies eligible updates serially.
type Executor struct {
	list Lister
	upd  updater.Updater
}

// NewExecutor builds an Executor over a status lister and an updater.
func NewExecutor(l Lister, u updater.Updater) *Executor { return &Executor{list: l, upd: u} }

// Run applies (or, in dryRun, would-apply) every eligible container's update,
// one at a time. A container on Policy.ExcludeContainers is never applied — it
// is reported as Skipped instead. A single failure is captured in its Outcome
// and never aborts the rest. It is a no-op (empty Result) when the updater is
// unsupported (e.g. the generic non-Unraid container).
func (e *Executor) Run(ctx context.Context, p Policy, dryRun bool) Result {
	res := Result{DryRun: dryRun}
	if !e.upd.Supported() {
		return res
	}
	statuses, err := e.list.List()
	if err != nil {
		return res
	}
	for _, st := range statuses {
		if !Eligible(st, p) {
			continue
		}
		o := Outcome{
			Name:  st.Container.Name,
			From:  st.RunningVersion,
			To:    st.NewestTag,
			Level: string(st.Kind),
		}
		if ContainerExcluded(o.Name, p.ExcludeContainers) {
			o.Skipped = true
			res.Outcomes = append(res.Outcomes, o)
			continue // never applied, in dry-run OR real mode — no Updater call either way
		}
		if word := MatchedExcludeWord(st.Changelog, p.ExcludeWords); word != "" {
			o.Blocked = true
			o.BlockedWord = word
			res.Outcomes = append(res.Outcomes, o)
			continue // never applied, in dry-run OR real mode — no Updater call either way
		}
		if dryRun {
			o.Updated = true // "would update"
		} else {
			o.Err = e.upd.Update(ctx, st.Container.Name)
			o.Updated = o.Err == nil
		}
		res.Outcomes = append(res.Outcomes, o)
	}
	return res
}
