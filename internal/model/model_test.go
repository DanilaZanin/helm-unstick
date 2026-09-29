package model

import (
	"reflect"
	"testing"
	"time"
)

func rev(n int, s Status) Revision { return Revision{Number: n, Status: s} }

func TestStatusIsPending(t *testing.T) {
	pending := map[Status]bool{
		StatusPendingInstall:  true,
		StatusPendingUpgrade:  true,
		StatusPendingRollback: true,
	}
	for _, s := range []Status{
		StatusUnknown, StatusDeployed, StatusUninstalled, StatusSuperseded, StatusFailed,
		StatusUninstalling, StatusPendingInstall, StatusPendingUpgrade, StatusPendingRollback,
	} {
		if got := s.IsPending(); got != pending[s] {
			t.Errorf("%s.IsPending() = %v, want %v", s, got, pending[s])
		}
	}
}

func TestAnalyze(t *testing.T) {
	tests := []struct {
		name         string
		revs         []Revision
		wantNil      bool
		wantPending  int
		wantTarget   int // 0 = no target
		wantRollback bool
		wantLeftover []int
	}{
		{name: "empty history", wantNil: true},
		{
			name:    "healthy release",
			revs:    []Revision{rev(1, StatusSuperseded), rev(2, StatusDeployed)},
			wantNil: true,
		},
		{
			name:    "failed last revision is not pending",
			revs:    []Revision{rev(1, StatusDeployed), rev(2, StatusFailed)},
			wantNil: true,
		},
		{
			name:         "interrupted upgrade",
			revs:         []Revision{rev(1, StatusSuperseded), rev(2, StatusDeployed), rev(3, StatusPendingUpgrade)},
			wantPending:  3,
			wantTarget:   2,
			wantRollback: true,
		},
		{
			name:         "history given out of order",
			revs:         []Revision{rev(3, StatusPendingUpgrade), rev(1, StatusSuperseded), rev(2, StatusDeployed)},
			wantPending:  3,
			wantTarget:   2,
			wantRollback: true,
		},
		{
			name:         "interrupted rollback",
			revs:         []Revision{rev(1, StatusSuperseded), rev(2, StatusDeployed), rev(3, StatusPendingRollback)},
			wantPending:  3,
			wantTarget:   2,
			wantRollback: true,
		},
		{
			name:         "failed upgrades between deployed and pending are skipped",
			revs:         []Revision{rev(1, StatusDeployed), rev(2, StatusFailed), rev(3, StatusFailed), rev(4, StatusPendingUpgrade)},
			wantPending:  4,
			wantTarget:   1,
			wantRollback: true,
		},
		{
			name:         "several deployed records: newest wins",
			revs:         []Revision{rev(1, StatusDeployed), rev(2, StatusDeployed), rev(3, StatusPendingUpgrade)},
			wantPending:  3,
			wantTarget:   2,
			wantRollback: true,
		},
		{
			name:         "superseded is not a rollback target",
			revs:         []Revision{rev(1, StatusSuperseded), rev(2, StatusFailed), rev(3, StatusPendingUpgrade)},
			wantPending:  3,
			wantRollback: false,
		},
		{
			name:         "interrupted first install",
			revs:         []Revision{rev(1, StatusPendingInstall)},
			wantPending:  1,
			wantRollback: false,
		},
		{
			name:         "pending-install never rolls back even if a deployed record exists",
			revs:         []Revision{rev(1, StatusDeployed), rev(2, StatusPendingInstall)},
			wantPending:  2,
			wantTarget:   1,
			wantRollback: false,
		},
		{
			name:         "older revisions left pending are reported",
			revs:         []Revision{rev(1, StatusDeployed), rev(2, StatusPendingUpgrade), rev(3, StatusPendingUpgrade)},
			wantPending:  3,
			wantTarget:   1,
			wantRollback: true,
			wantLeftover: []int{2},
		},
		{
			name:    "old pending record under a deployed latest revision is not stuck",
			revs:    []Revision{rev(1, StatusPendingUpgrade), rev(2, StatusDeployed)},
			wantNil: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Analyze(History{Namespace: "ns", Release: "web", Revisions: tt.revs})
			if tt.wantNil {
				if got != nil {
					t.Fatalf("Analyze() = %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("Analyze() = nil, want stuck release")
			}
			if got.Namespace != "ns" || got.Release != "web" {
				t.Errorf("identity = %s/%s", got.Namespace, got.Release)
			}
			if got.Pending.Number != tt.wantPending {
				t.Errorf("Pending = %d, want %d", got.Pending.Number, tt.wantPending)
			}
			gotTarget := 0
			if got.Target != nil {
				gotTarget = got.Target.Number
			}
			if gotTarget != tt.wantTarget {
				t.Errorf("Target = %d, want %d", gotTarget, tt.wantTarget)
			}
			if got.HasRollbackTarget() != tt.wantRollback {
				t.Errorf("HasRollbackTarget() = %v, want %v", got.HasRollbackTarget(), tt.wantRollback)
			}
			var leftover []int
			for _, r := range got.Leftover {
				leftover = append(leftover, r.Number)
			}
			if !reflect.DeepEqual(leftover, tt.wantLeftover) {
				t.Errorf("Leftover = %v, want %v", leftover, tt.wantLeftover)
			}
			if got.NextRevision() != tt.wantPending+1 {
				t.Errorf("NextRevision() = %d", got.NextRevision())
			}
			for i := 1; i < len(got.Revisions); i++ {
				if got.Revisions[i-1].Number >= got.Revisions[i].Number {
					t.Errorf("Revisions not ascending: %v", got.Revisions)
				}
			}
		})
	}
}

func TestAnalyzeDoesNotModifyInput(t *testing.T) {
	in := []Revision{rev(3, StatusPendingUpgrade), rev(1, StatusSuperseded), rev(2, StatusDeployed)}
	Analyze(History{Revisions: in})
	if in[0].Number != 3 || in[1].Number != 1 || in[2].Number != 2 {
		t.Errorf("input reordered: %v", in)
	}
}

func TestLastActivity(t *testing.T) {
	t0 := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(5 * time.Minute)
	tests := []struct {
		name string
		r    Revision
		want time.Time
	}{
		{"only updated", Revision{Updated: t0}, t0},
		{"only modified", Revision{ModifiedAt: t1}, t1},
		{"modified later", Revision{Updated: t0, ModifiedAt: t1}, t1},
		{"updated later", Revision{Updated: t1, ModifiedAt: t0}, t1},
		{"neither", Revision{}, time.Time{}},
	}
	for _, tt := range tests {
		if got := tt.r.LastActivity(); !got.Equal(tt.want) {
			t.Errorf("%s: LastActivity() = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestHistoryLatest(t *testing.T) {
	if _, ok := (History{}).Latest(); ok {
		t.Error("empty history has no latest revision")
	}
	h := History{Revisions: []Revision{rev(2, StatusDeployed), rev(4, StatusFailed), rev(3, StatusSuperseded)}}
	got, ok := h.Latest()
	if !ok || got.Number != 4 {
		t.Errorf("Latest() = %+v, %v", got, ok)
	}
}
