package render

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/DanilaZanin/helm-unstick/internal/model"
	"github.com/DanilaZanin/helm-unstick/internal/plan"
	"github.com/DanilaZanin/helm-unstick/internal/verdict"
)

func upgradeStuck() *model.Stuck {
	t0 := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	return model.Analyze(model.History{Namespace: "prod", Release: "web", Revisions: []model.Revision{
		{Number: 5, Status: model.StatusSuperseded, Updated: t0.Add(-48 * time.Hour), Chart: "web-1.0.0", Description: "Upgrade complete"},
		{Number: 6, Status: model.StatusDeployed, Updated: t0.Add(-24 * time.Hour), Chart: "web-1.1.0", Description: "Upgrade complete"},
		{Number: 7, Status: model.StatusPendingUpgrade, Updated: t0, Chart: "web-1.2.0", Description: "Preparing upgrade"},
	}})
}

func TestScanTable(t *testing.T) {
	s := upgradeStuck()
	res := verdict.Result{Verdict: verdict.Stale, Reasons: []string{"old"}}
	rows := []ScanRow{NewScanRow(s, 2*time.Hour+13*time.Minute, res)}
	var buf bytes.Buffer
	if err := ScanTable(&buf, rows); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want header and one row, got:\n%s", out)
	}
	for _, want := range []string{"NAMESPACE", "VERDICT", "ACTION"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("header missing %s: %q", want, lines[0])
		}
	}
	fields := strings.Fields(lines[1])
	// prod web pending-upgrade 7 2h13m 6 stale fix: roll back to revision 6
	wantPrefix := []string{"prod", "web", "pending-upgrade", "7", "2h13m", "6", "stale", "fix:"}
	for i, want := range wantPrefix {
		if fields[i] != want {
			t.Errorf("column %d = %q, want %q (row %q)", i, fields[i], want, lines[1])
		}
	}
}

func TestScanTableNoTarget(t *testing.T) {
	s := model.Analyze(model.History{Namespace: "ns", Release: "new", Revisions: []model.Revision{{Number: 1, Status: model.StatusPendingInstall}}})
	var buf bytes.Buffer
	_ = ScanTable(&buf, []ScanRow{NewScanRow(s, time.Minute, verdict.Result{Verdict: verdict.PossiblyRunning})})
	fields := strings.Fields(strings.Split(buf.String(), "\n")[1])
	if fields[5] != "-" {
		t.Errorf("LAST-DEPLOYED = %q, want -", fields[5])
	}
}

func TestScanJSON(t *testing.T) {
	s := upgradeStuck()
	row := NewScanRow(s, 90*time.Second, verdict.Result{Verdict: verdict.PossiblyRunning, Reasons: []string{"young"}})
	var buf bytes.Buffer
	if err := ScanJSON(&buf, []ScanRow{row}); err != nil {
		t.Fatal(err)
	}
	// e2e greps for this exact spelling.
	if !strings.Contains(buf.String(), `"verdict": "possibly-running"`) {
		t.Errorf("JSON is not indented as documented:\n%s", buf.String())
	}
	var back []ScanRow
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatal(err)
	}
	if len(back) != 1 || back[0].Release != "web" || back[0].AgeSeconds != 90 ||
		back[0].LastDeployedRevision == nil || *back[0].LastDeployedRevision != 6 {
		t.Errorf("round trip: %+v", back)
	}
}

func TestScanJSONEmptyIsArray(t *testing.T) {
	var buf bytes.Buffer
	if err := ScanJSON(&buf, nil); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(buf.String()) != "[]" {
		t.Errorf("empty scan = %q, want []", buf.String())
	}
}

func TestScanRowNullTarget(t *testing.T) {
	s := model.Analyze(model.History{Namespace: "ns", Release: "new", Revisions: []model.Revision{{Number: 1, Status: model.StatusPendingInstall}}})
	var buf bytes.Buffer
	_ = ScanJSON(&buf, []ScanRow{NewScanRow(s, time.Minute, verdict.Result{Verdict: verdict.Stale})})
	if !strings.Contains(buf.String(), `"lastDeployedRevision": null`) {
		t.Errorf("missing target must be null:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), `"reasons": []`) {
		t.Errorf("reasons must never be null:\n%s", buf.String())
	}
}

func TestExplain(t *testing.T) {
	s := upgradeStuck()
	res := verdict.Result{Verdict: verdict.PossiblyRunning, Reasons: []string{"Deployment/web: rollout in progress: 1 of 3 replicas updated"}}
	var buf bytes.Buffer
	Explain(&buf, Report{
		Stuck: s, Age: 2 * time.Hour, OlderThan: 10 * time.Minute, Verdict: res,
		Plan: plan.Build(s, plan.FirstInstallNone, plan.Context{Tool: "helm-unstick"}),
	})
	out := buf.String()
	for _, want := range []string{
		"Release:   web (namespace prod)",
		"Status:    pending-upgrade, revision 7",
		"revision 6 is the newest deployed revision",
		"Verdict:   possibly-running",
		"Deployment/web: rollout in progress",
		"Roll back to revision 6",
		"helm rollback web 6 -n prod",
		"fix would refuse right now",
		"History:",
		"web-1.1.0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("explain output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\u2014") {
		t.Error("output must not contain em dashes")
	}
}

func TestExplainStaleHasNoRefusalNote(t *testing.T) {
	s := upgradeStuck()
	var buf bytes.Buffer
	Explain(&buf, Report{Stuck: s, Age: time.Hour, Verdict: verdict.Result{Verdict: verdict.Stale, Reasons: []string{"old"}},
		Plan: plan.Build(s, plan.FirstInstallNone, plan.Context{Tool: "helm-unstick"})})
	if strings.Contains(buf.String(), "would refuse") {
		t.Errorf("stale explain must not say fix refuses:\n%s", buf.String())
	}
}
