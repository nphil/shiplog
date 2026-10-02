package autoupdate

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/junkerderprovinz/shiplog/internal/model"
	"github.com/junkerderprovinz/shiplog/internal/updater"
)

// Lister supplies the current per-container update statuses (the store).
type Lister interface {
	List() ([]model.UpdateStatus, error)
}

// Inspector reads the containers as they run right now (the read-only Docker
// socket client). The stored statuses can be hours old, so the executor looks
// at a container itself just before and just after an update: that is what
// keeps a no-op from being reported as an update and a failed update from being
// reported as a success.
type Inspector interface {
	List(ctx context.Context) ([]model.Container, error)
}

// ErrNoChange is the failure reported when the updater says it succeeded but the
// container still runs the very same image as before.
var ErrNoChange = errors.New("update did not change the image")

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
	// UpToDate is set when the update was level-eligible on paper but the
	// container, looked at live right before the update, already runs the image
	// the registry serves for its tag — the stored verdict was stale, so there is
	// nothing to do. Like Blocked and Skipped it is a deliberate non-action:
	// Updated false, Err nil, no Updater call, real and dry-run mode alike. It is
	// never counted as a success, because nothing was updated.
	UpToDate bool
}

// Result aggregates one auto-update run.
type Result struct {
	Outcomes []Outcome
	DryRun   bool
}

// Notable reports whether the run is worth pushing to the notification
// channels. A run whose only outcomes are containers the admin excluded on
// purpose, or that turned out to be up to date already, is not: they are
// already told, in the run log, that nothing was done for those containers, and
// a daily ping about them is noise.
func (r Result) Notable() bool {
	for _, o := range r.Outcomes {
		if !o.Skipped && !o.UpToDate {
			return true
		}
	}
	return false
}

// Executor applies eligible updates serially.
type Executor struct {
	list   Lister
	docker Inspector
	upd    updater.Updater
}

// NewExecutor builds an Executor over a status lister, a live container
// inspector and an updater.
func NewExecutor(l Lister, in Inspector, u updater.Updater) *Executor {
	return &Executor{list: l, docker: in, upd: u}
}

// Run applies (or, in dryRun, would-apply) every eligible container's update,
// one at a time. A container on Policy.ExcludeContainers is never applied — it
// is reported as Skipped instead. A container that already runs the newest
// image of its tag is reported as UpToDate and left alone. After a real update
// the container is inspected again and the update only counts as done when it
// now runs a different image, namely the expected one. A single failure is
// captured in its Outcome and never aborts the rest. It is a no-op (empty
// Result) when the updater is unsupported (e.g. the generic non-Unraid
// container).
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
		before, ierr := e.running(ctx, o.Name)
		switch {
		case ierr != nil:
			// Never update what cannot be looked at: without the "before" picture
			// the result could not be verified either.
			o.Err = fmt.Errorf("cannot inspect the container before updating: %w", ierr)
		case before.HasDigest(st.NewestDigest):
			o.UpToDate = true // already on the newest image of its tag — nothing to pull
		case dryRun:
			o.Updated = true // "would update"
		default:
			o.Err = e.update(ctx, o.Name, before, st.NewestDigest)
			o.Updated = o.Err == nil
		}
		res.Outcomes = append(res.Outcomes, o)
	}
	return res
}

// update runs the updater for name and then proves the update took effect.
func (e *Executor) update(ctx context.Context, name string, before model.Container, want string) error {
	if err := e.upd.Update(ctx, name); err != nil {
		return err
	}
	return e.verify(ctx, name, before, want)
}

// verify proves an update the updater reported as successful: the container
// must now run a DIFFERENT image than it did before, and — when the expected
// digest is known — exactly that one. An updater whose script exits 0 after a
// re-pull of an unchanged tag (or after failing to recreate the container)
// leaves the image as it was, and that must never read as a success.
func (e *Executor) verify(ctx context.Context, name string, before model.Container, want string) error {
	after, err := e.running(ctx, name)
	if err != nil {
		return fmt.Errorf("update ran but could not be verified: %w", err)
	}
	if sharesImage(before, after) {
		return ErrNoChange
	}
	if after.Digest == "" {
		return errors.New("update ran but the container's new image digest could not be read")
	}
	if want != "" && !after.HasDigest(want) {
		return fmt.Errorf("update changed the image, but it now runs %s instead of the expected %s",
			shortDigest(after.Digest), shortDigest(want))
	}
	return nil
}

// running returns the named container as it runs right now.
func (e *Executor) running(ctx context.Context, name string) (model.Container, error) {
	cs, err := e.docker.List(ctx)
	if err != nil {
		return model.Container{}, err
	}
	for _, c := range cs {
		if c.Name == name {
			return c, nil
		}
	}
	return model.Container{}, fmt.Errorf("container %q not found", name)
}

// sharesImage reports whether a and b run the same image: any manifest digest
// (an image pulled through a mirror carries one per registry) in common.
func sharesImage(a, b model.Container) bool {
	if a.HasDigest(b.Digest) {
		return true
	}
	for _, d := range b.Digests {
		if a.HasDigest(d) {
			return true
		}
	}
	return false
}

// shortDigest renders a digest the way `docker images --digests` does, cut to
// 12 hex characters; "?" when unknown.
func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	switch {
	case d == "":
		return "?"
	case len(d) > 12:
		return d[:12]
	}
	return d
}
