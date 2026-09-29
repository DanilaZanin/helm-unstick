package verdict

import (
	"strings"
	"testing"
	"time"
)

func TestDecide(t *testing.T) {
	activity := Signal{Kind: SignalRollout, Object: "Deployment/web", Detail: "1 of 3 replicas updated"}
	tests := []struct {
		name string
		in   Input
		want Verdict
	}{
		{
			name: "old and quiet is stale",
			in:   Input{Age: time.Hour, OlderThan: 10 * time.Minute, Evidence: Evidence{Checked: 4}},
			want: Stale,
		},
		{
			name: "younger than threshold is possibly-running",
			in:   Input{Age: 3 * time.Minute, OlderThan: 10 * time.Minute, Evidence: Evidence{Checked: 4}},
			want: PossiblyRunning,
		},
		{
			name: "exactly at threshold is stale",
			in:   Input{Age: 10 * time.Minute, OlderThan: 10 * time.Minute},
			want: Stale,
		},
		{
			name: "old but a rollout is in progress",
			in:   Input{Age: time.Hour, OlderThan: 10 * time.Minute, Evidence: Evidence{Signals: []Signal{activity}}},
			want: PossiblyRunning,
		},
		{
			name: "old but incomplete evidence is unknown",
			in:   Input{Age: time.Hour, OlderThan: 10 * time.Minute, Evidence: Evidence{Errors: []string{"forbidden"}}},
			want: Unknown,
		},
		{
			name: "young and incomplete evidence stays possibly-running, not unknown",
			in:   Input{Age: time.Minute, OlderThan: 10 * time.Minute, Evidence: Evidence{Errors: []string{"forbidden"}}},
			want: PossiblyRunning,
		},
		{
			name: "signal beats incomplete evidence",
			in:   Input{Age: time.Hour, OlderThan: time.Minute, Evidence: Evidence{Signals: []Signal{activity}, Errors: []string{"forbidden"}}},
			want: PossiblyRunning,
		},
		{
			name: "zero threshold with quiet cluster is stale",
			in:   Input{Age: time.Second, OlderThan: 0},
			want: Stale,
		},
		{
			name: "zero threshold does not hide a rollout",
			in:   Input{Age: time.Second, OlderThan: 0, Evidence: Evidence{Signals: []Signal{activity}}},
			want: PossiblyRunning,
		},
		{
			name: "future timestamp with a threshold is possibly-running",
			in:   Input{Age: -time.Hour, OlderThan: 10 * time.Minute},
			want: PossiblyRunning,
		},
		{
			name: "future timestamp with zero threshold is stale",
			in:   Input{Age: -time.Hour, OlderThan: 0},
			want: Stale,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Decide(tt.in)
			if got.Verdict != tt.want {
				t.Fatalf("Verdict = %s, want %s (reasons: %v)", got.Verdict, tt.want, got.Reasons)
			}
			if len(got.Reasons) == 0 {
				t.Error("every verdict must carry at least one reason")
			}
		})
	}
}

func TestDecideReasonsMentionCause(t *testing.T) {
	got := Decide(Input{
		Age: time.Hour, OlderThan: 10 * time.Minute,
		Evidence: Evidence{Signals: []Signal{{Kind: SignalJobRunning, Object: "Job/web-migrate", Detail: "1 active pod(s)"}}},
	})
	joined := strings.Join(got.Reasons, "\n")
	if !strings.Contains(joined, "Job/web-migrate: 1 active pod(s)") {
		t.Errorf("reasons do not name the signal:\n%s", joined)
	}

	got = Decide(Input{Age: time.Minute, OlderThan: 10 * time.Minute})
	if !strings.Contains(strings.Join(got.Reasons, "\n"), "less than the 10m threshold") {
		t.Errorf("reasons do not explain the age rule: %v", got.Reasons)
	}

	got = Decide(Input{Age: time.Hour, OlderThan: 10 * time.Minute})
	if !strings.Contains(strings.Join(got.Reasons, "\n"), "none of the release objects exists") {
		t.Errorf("a release with no live objects must say so: %v", got.Reasons)
	}

	got = Decide(Input{Age: time.Hour, OlderThan: 10 * time.Minute, Evidence: Evidence{Checked: 3}})
	if !strings.Contains(strings.Join(got.Reasons, "\n"), "none of the 3 release objects found in the cluster changed in the last 10m") {
		t.Errorf("stale reasons must count the objects: %v", got.Reasons)
	}

	got = Decide(Input{Age: time.Hour, OlderThan: time.Minute, Evidence: Evidence{Errors: []string{"list jobs: forbidden"}}})
	if !strings.Contains(strings.Join(got.Reasons, "\n"), "could not check: list jobs: forbidden") {
		t.Errorf("reasons do not carry the error: %v", got.Reasons)
	}
}

func TestAllows(t *testing.T) {
	tests := []struct {
		name  string
		v     Verdict
		force bool
		want  bool
		note  bool // expect a non-empty why
	}{
		{"stale", Stale, false, true, false},
		{"stale with force", Stale, true, true, false},
		{"unknown refused", Unknown, false, false, true},
		{"unknown forced", Unknown, true, true, true},
		{"possibly-running refused", PossiblyRunning, false, false, true},
		{"possibly-running cannot be forced", PossiblyRunning, true, false, true},
	}
	for _, tt := range tests {
		ok, why := Result{Verdict: tt.v}.Allows(tt.force)
		if ok != tt.want {
			t.Errorf("%s: Allows = %v, want %v", tt.name, ok, tt.want)
		}
		if (why != "") != tt.note {
			t.Errorf("%s: why = %q", tt.name, why)
		}
	}
}

func TestBoundedSignalsExpireWithHelmTimeout(t *testing.T) {
	wait := Signal{Kind: SignalNotReady, Object: "Deployment/web", Detail: "helm --wait may still be waiting: 0 of 1 updated replicas available", Bounded: true}
	tests := []struct {
		name string
		in   Input
		want Verdict
	}{
		{"inside the Helm timeout", Input{Age: 3 * time.Minute, HelmTimeout: 5 * time.Minute, Evidence: Evidence{Signals: []Signal{wait}}}, PossiblyRunning},
		{"inside the grace period", Input{Age: 5*time.Minute + 30*time.Second, HelmTimeout: 5 * time.Minute, Evidence: Evidence{Signals: []Signal{wait}}}, PossiblyRunning},
		{"past timeout and grace", Input{Age: 7 * time.Minute, HelmTimeout: 5 * time.Minute, Evidence: Evidence{Signals: []Signal{wait}}}, Stale},
		{"past the timeout but younger than --older-than", Input{Age: 7 * time.Minute, OlderThan: 10 * time.Minute, HelmTimeout: 5 * time.Minute, Evidence: Evidence{Signals: []Signal{wait}}}, PossiblyRunning},
		{"a longer Helm timeout keeps blocking", Input{Age: 7 * time.Minute, HelmTimeout: 30 * time.Minute, Evidence: Evidence{Signals: []Signal{wait}}}, PossiblyRunning},
		{"an unbounded signal never expires", Input{Age: 5 * time.Hour, HelmTimeout: 5 * time.Minute, Evidence: Evidence{Signals: []Signal{{Kind: SignalRecentChange, Object: "ConfigMap/x", Detail: "modified 1s ago"}}}}, PossiblyRunning},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Decide(tt.in)
			if got.Verdict != tt.want {
				t.Fatalf("Verdict = %s, want %s (reasons: %v)", got.Verdict, tt.want, got.Reasons)
			}
		})
	}
	stale := Decide(Input{Age: 7 * time.Minute, HelmTimeout: 5 * time.Minute, Evidence: Evidence{Signals: []Signal{wait}}})
	joined := strings.Join(stale.Reasons, "\n")
	if !strings.Contains(joined, "Deployment/web") || !strings.Contains(joined, "given up") {
		t.Errorf("a stale verdict must say which not-ready objects it disregarded and why:\n%s", joined)
	}
	running := Decide(Input{Age: 3 * time.Minute, HelmTimeout: 5 * time.Minute, Evidence: Evidence{Signals: []Signal{wait}}})
	if !strings.Contains(strings.Join(running.Reasons, "\n"), "--helm-timeout") {
		t.Errorf("a blocking wait signal must point at --helm-timeout: %v", running.Reasons)
	}
}

func TestRefusalDoesNotSuggestRaisingOlderThan(t *testing.T) {
	_, why := Result{Verdict: PossiblyRunning}.Allows(false)
	if strings.Contains(why, "raise") {
		t.Errorf("raising --older-than widens the refusal, it never lifts it: %q", why)
	}
}
