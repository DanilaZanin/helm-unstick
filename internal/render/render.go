// Package render writes scan tables, JSON and the explain report.
package render

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/DanilaZanin/helm-unstick/internal/humanize"
	"github.com/DanilaZanin/helm-unstick/internal/model"
	"github.com/DanilaZanin/helm-unstick/internal/plan"
	"github.com/DanilaZanin/helm-unstick/internal/verdict"
)

// ScanRow is one stuck release in the scan output.
type ScanRow struct {
	Namespace            string   `json:"namespace"`
	Release              string   `json:"release"`
	Status               string   `json:"status"`
	Revision             int      `json:"revision"`
	Age                  string   `json:"age"`
	AgeSeconds           int64    `json:"ageSeconds"`
	LastDeployedRevision *int     `json:"lastDeployedRevision"`
	Verdict              string   `json:"verdict"`
	RecommendedAction    string   `json:"recommendedAction"`
	Reasons              []string `json:"reasons"`
}

// NewScanRow assembles a row from an analyzed release and its verdict.
func NewScanRow(s *model.Stuck, age time.Duration, res verdict.Result) ScanRow {
	row := ScanRow{
		Namespace:         s.Namespace,
		Release:           s.Release,
		Status:            string(s.Pending.Status),
		Revision:          s.Pending.Number,
		Age:               humanize.Duration(age),
		AgeSeconds:        int64(max(age, 0) / time.Second),
		Verdict:           string(res.Verdict),
		RecommendedAction: plan.Recommend(s, res.Verdict),
		Reasons:           res.Reasons,
	}
	if row.Reasons == nil {
		row.Reasons = []string{}
	}
	if s.Target != nil {
		n := s.Target.Number
		row.LastDeployedRevision = &n
	}
	return row
}

// ScanJSON writes rows as an indented JSON array ("[]" when empty).
func ScanJSON(w io.Writer, rows []ScanRow) error {
	if rows == nil {
		rows = []ScanRow{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rows)
}

// ScanTable writes rows as an aligned table.
func ScanTable(w io.Writer, rows []ScanRow) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAMESPACE\tRELEASE\tSTATUS\tREV\tAGE\tLAST-DEPLOYED\tVERDICT\tACTION")
	for _, r := range rows {
		last := "-"
		if r.LastDeployedRevision != nil {
			last = fmt.Sprint(*r.LastDeployedRevision)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			r.Namespace, r.Release, r.Status, r.Revision, r.Age, last, r.Verdict, r.RecommendedAction)
	}
	return tw.Flush()
}

// Report is everything explain and fix show about one stuck release.
type Report struct {
	Stuck     *model.Stuck
	Age       time.Duration
	OlderThan time.Duration
	Verdict   verdict.Result
	Plan      plan.Plan
}

// Summary writes the facts and the verdict with its reasons.
func Summary(w io.Writer, r Report) {
	s := r.Stuck
	fmt.Fprintf(w, "Release:   %s (namespace %s)\n", s.Release, s.Namespace)
	fmt.Fprintf(w, "Status:    %s, revision %d, last written %s ago\n", s.Pending.Status, s.Pending.Number, humanize.Duration(r.Age))
	if s.Pending.Chart != "" {
		fmt.Fprintf(w, "Chart:     %s\n", s.Pending.Chart)
	}
	if s.Target != nil {
		fmt.Fprintf(w, "Deployed:  revision %d is the newest deployed revision\n", s.Target.Number)
	} else {
		fmt.Fprintln(w, "Deployed:  no deployed revision in the history")
	}
	fmt.Fprintf(w, "Verdict:   %s\n", r.Verdict.Verdict)
	for _, reason := range r.Verdict.Reasons {
		fmt.Fprintf(w, "  - %s\n", reason)
	}
}

// Plan writes the recovery plan.
func Plan(w io.Writer, p plan.Plan) {
	fmt.Fprintf(w, "Plan:      %s\n", p.Summary)
	for i, step := range p.Steps {
		fmt.Fprintf(w, "  %d. %s\n", i+1, step)
	}
	for _, warn := range p.Warnings {
		fmt.Fprintf(w, "  ! %s\n", warn)
	}
	if len(p.Commands) > 0 {
		fmt.Fprintln(w, "Commands:")
		for _, c := range p.Commands {
			fmt.Fprintf(w, "  %s\n    # %s\n", c.Line, c.Note)
		}
	}
}

// History writes the revision table.
func History(w io.Writer, revs []model.Revision) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  REV\tSTATUS\tUPDATED\tCHART\tDESCRIPTION")
	for _, r := range revs {
		updated := "-"
		if !r.Updated.IsZero() {
			updated = r.Updated.UTC().Format("2006-01-02 15:04:05Z")
		}
		fmt.Fprintf(tw, "  %d\t%s\t%s\t%s\t%s\n", r.Number, r.Status, updated, dash(r.Chart), dash(strings.TrimSpace(r.Description)))
	}
	_ = tw.Flush()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// Explain writes the full explain report.
func Explain(w io.Writer, r Report) {
	Summary(w, r)
	fmt.Fprintln(w)
	Plan(w, r.Plan)
	if ok, why := r.Verdict.Allows(false); !ok {
		fmt.Fprintf(w, "\nNote:      fix would refuse right now: %s\n", why)
	}
	fmt.Fprintln(w, "\nHistory:")
	History(w, r.Stuck.Revisions)
}
