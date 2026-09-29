// Package model describes a Helm release history independently of where it is stored
// and decides whether the release is stuck in a pending-* state.
package model

import (
	"errors"
	"sort"
	"time"
)

// ErrNotFound is returned (wrapped) when a release does not exist.
var ErrNotFound = errors.New("release not found")

// ErrConflict is returned (wrapped) when a release record is no longer the one that was
// inspected: someone wrote to it, so the caller's verdict is void.
var ErrConflict = errors.New("release record changed since it was inspected")

// ErrPending is returned (wrapped) when Helm refuses an operation because the release
// already has a pending-* revision ("another operation is in progress").
var ErrPending = errors.New("helm refused: the release has a pending revision")

// Status is the Helm release status string (info.status, secret label "status").
type Status string

// Release statuses as written by Helm 3 and Helm 4.
const (
	StatusUnknown         Status = "unknown"
	StatusDeployed        Status = "deployed"
	StatusUninstalled     Status = "uninstalled"
	StatusSuperseded      Status = "superseded"
	StatusFailed          Status = "failed"
	StatusUninstalling    Status = "uninstalling"
	StatusPendingInstall  Status = "pending-install"
	StatusPendingUpgrade  Status = "pending-upgrade"
	StatusPendingRollback Status = "pending-rollback"
)

// PendingStatuses lists every status that means "an operation is (or was) in progress".
var PendingStatuses = []Status{StatusPendingInstall, StatusPendingUpgrade, StatusPendingRollback}

// IsPending reports whether s is one of the pending-* statuses.
func (s Status) IsPending() bool {
	switch s {
	case StatusPendingInstall, StatusPendingUpgrade, StatusPendingRollback:
		return true
	}
	return false
}

// Revision is one entry of a release history.
type Revision struct {
	Number      int
	Status      Status
	Updated     time.Time // info.last_deployed: when the operation that wrote this revision started
	ModifiedAt  time.Time // "modifiedAt" label of the storage record; zero when unknown
	Version     string    // opaque version of the storage record (resourceVersion); guards writes
	Chart       string    // name-version
	Description string
}

// LastActivity is the latest known write to this revision. Taking the later of the two
// timestamps errs on the side of "recently active", which is the safe direction.
func (r Revision) LastActivity() time.Time {
	if r.ModifiedAt.After(r.Updated) {
		return r.ModifiedAt
	}
	return r.Updated
}

// History is every stored revision of one release, in any order.
type History struct {
	Namespace string
	Release   string
	Revisions []Revision
}

// Latest returns the revision with the highest number.
func (h History) Latest() (Revision, bool) {
	if len(h.Revisions) == 0 {
		return Revision{}, false
	}
	best := h.Revisions[0]
	for _, r := range h.Revisions[1:] {
		if r.Number > best.Number {
			best = r
		}
	}
	return best, true
}

// Problem is a release that scan could not read completely, for example because one of its
// storage records does not decode.
type Problem struct {
	Namespace string
	Release   string
	Err       string
}

// Listing is the result of looking for pending releases: the histories that were read and
// the releases that could not be read. A listing with problems is incomplete.
type Listing struct {
	Histories []History
	Problems  []Problem
}

// Stuck describes a release whose latest revision is pending-*.
type Stuck struct {
	Namespace string
	Release   string
	Pending   Revision  // the latest revision
	Target    *Revision // newest deployed revision older than Pending; nil when there is none
	// Superseded is the newest superseded revision older than Pending, set only when Target
	// is nil. Helm stores the old revision as superseded before it stores the new deployed
	// one, so an interruption between the two writes leaves history with no deployed revision.
	Superseded *Revision
	Leftover   []Revision
	Revisions  []Revision // whole history, ascending
}

// ActionOptions tune the Helm operations helm-unstick performs.
type ActionOptions struct {
	Wait    bool
	Timeout time.Duration
}

// Analyze returns nil when the latest revision is not pending. Only the latest revision
// matters: Helm decides "another operation is in progress" from it alone.
func Analyze(h History) *Stuck {
	if len(h.Revisions) == 0 {
		return nil
	}
	revs := append([]Revision(nil), h.Revisions...)
	sort.Slice(revs, func(i, j int) bool { return revs[i].Number < revs[j].Number })
	last := revs[len(revs)-1]
	if !last.Status.IsPending() {
		return nil
	}
	s := &Stuck{Namespace: h.Namespace, Release: h.Release, Pending: last, Revisions: revs}
	for i := len(revs) - 2; i >= 0; i-- {
		r := revs[i]
		if s.Target == nil && r.Status == StatusDeployed {
			target := r
			s.Target = &target
		}
		if r.Status.IsPending() {
			s.Leftover = append(s.Leftover, r)
		}
	}
	sort.Slice(s.Leftover, func(i, j int) bool { return s.Leftover[i].Number < s.Leftover[j].Number })
	if s.Target == nil {
		for i := len(revs) - 2; i >= 0; i-- {
			if revs[i].Status == StatusSuperseded {
				r := revs[i]
				s.Superseded = &r
				break
			}
		}
	}
	return s
}

// RollbackCandidate returns revision n if it may be rolled back to on request: it must be
// older than the pending revision and must have been deployed at some point.
func (s *Stuck) RollbackCandidate(n int) (Revision, bool) {
	for _, r := range s.Revisions {
		if r.Number == n && n < s.Pending.Number && (r.Status == StatusDeployed || r.Status == StatusSuperseded) {
			return r, true
		}
	}
	return Revision{}, false
}

// HasRollbackTarget reports whether "roll back to the last deployed revision" is a valid
// recovery. An interrupted first install never has one, whatever else is in the history.
func (s *Stuck) HasRollbackTarget() bool {
	return s.Pending.Status != StatusPendingInstall && s.Target != nil
}

// NextRevision is the number Helm will give to the record a rollback creates.
func (s *Stuck) NextRevision() int { return s.Pending.Number + 1 }
