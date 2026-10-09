// Package report aggregates stored results into a CLI summary. The aggregation
// is kept separate from storage so a web dashboard can reuse the same records.
package report

import (
	"fmt"
	"io"
	"sort"

	"github.com/mhenni13/ci-eval-llm/internal/results"
)

type Summary struct {
	Source       string // "", "ci", "runtime"
	Total        int
	Passed       int
	Failed       int
	Errors       int
	Skipped      int
	AvgScore     float64
	PassRate     float64
	Distribution [10]int // score buckets 0.0-0.1 ... 0.9-1.0
	AvgLatencyMS float64
	Satisfaction *float64 // runtime only, nil if no feedback
	FeedbackN    int
	Failures     []results.Result
}

// Build aggregates records, optionally filtering by source.
func Build(rs []results.Result, source string) Summary {
	s := Summary{Source: source}
	var scoreSum, latSum float64
	var scored int
	var satSum, satN int
	for _, r := range rs {
		if source != "" && r.Source != source {
			continue
		}
		s.Total++
		switch r.Status {
		case results.Pass:
			s.Passed++
		case results.Fail:
			s.Failed++
			s.Failures = append(s.Failures, r)
		case results.Error:
			s.Errors++
			s.Failures = append(s.Failures, r)
		case results.Skipped:
			s.Skipped++
		}
		if r.Status == results.Pass || r.Status == results.Fail {
			scoreSum += r.Score
			scored++
			b := int(r.Score * 10)
			if b > 9 {
				b = 9
			}
			if b < 0 {
				b = 0
			}
			s.Distribution[b]++
		}
		latSum += float64(r.LatencyMS)
		if r.Satisfaction != nil {
			satSum += *r.Satisfaction
			satN++
		}
	}
	if scored > 0 {
		s.AvgScore = scoreSum / float64(scored)
	}
	evaluated := s.Passed + s.Failed
	if evaluated > 0 {
		s.PassRate = float64(s.Passed) / float64(evaluated) * 100
	}
	if s.Total > 0 {
		s.AvgLatencyMS = latSum / float64(s.Total)
	}
	if satN > 0 {
		avg := float64(satSum) / float64(satN)
		s.Satisfaction = &avg
		s.FeedbackN = satN
	}
	return s
}

// RunCount returns the number of distinct historical CI runs.
func RunCount(rs []results.Result) int {
	seen := map[string]bool{}
	for _, r := range rs {
		if r.Source == "ci" && r.RunID != "" {
			seen[r.RunID] = true
		}
	}
	return len(seen)
}

// WriteText prints a human report.
func WriteText(w io.Writer, rs []results.Result) {
	ci := Build(rs, "ci")
	rt := Build(rs, "runtime")

	fmt.Fprintln(w, "LLM Evaluation Report")
	fmt.Fprintln(w, "─────────────────────")
	fmt.Fprintf(w, "Historical CI runs: %d\n\n", RunCount(rs))

	writeSection(w, "CI", ci)
	if rt.Total > 0 {
		fmt.Fprintln(w)
		writeSection(w, "Runtime", rt)
		if rt.Satisfaction != nil {
			fmt.Fprintf(w, "User satisfaction:  %.2f (n=%d)  [separate signal from quality]\n", *rt.Satisfaction, rt.FeedbackN)
		}
	}

	all := Build(rs, "")
	if len(all.Failures) > 0 {
		fmt.Fprintln(w, "\nFailures & errors:")
		// Most recent first.
		fails := append([]results.Result(nil), all.Failures...)
		sort.Slice(fails, func(i, j int) bool { return fails[i].Timestamp.After(fails[j].Timestamp) })
		max := 20
		for i, f := range fails {
			if i >= max {
				fmt.Fprintf(w, "  ... and %d more\n", len(fails)-max)
				break
			}
			if f.Status == results.Error {
				fmt.Fprintf(w, "  ERROR %-24s %s\n", f.TestID, f.Error)
			} else {
				fmt.Fprintf(w, "  FAIL  %-24s score %.2f < %.2f\n", f.TestID, f.Score, f.Threshold)
				for _, c := range f.Criteria {
					if !c.Pass {
						fmt.Fprintf(w, "          ✗ %s (%s)\n", critLabel(c), c.Detail())
					}
				}
			}
		}
	}
}

func writeSection(w io.Writer, title string, s Summary) {
	if s.Total == 0 {
		fmt.Fprintf(w, "%s: no evaluations\n", title)
		return
	}
	fmt.Fprintf(w, "%s\n", title)
	fmt.Fprintf(w, "  Total:       %d\n", s.Total)
	fmt.Fprintf(w, "  Passed:      %d\n", s.Passed)
	fmt.Fprintf(w, "  Failed:      %d\n", s.Failed)
	fmt.Fprintf(w, "  Errors:      %d\n", s.Errors)
	if s.Skipped > 0 {
		fmt.Fprintf(w, "  Skipped:     %d\n", s.Skipped)
	}
	fmt.Fprintf(w, "  Pass rate:   %.1f%%\n", s.PassRate)
	fmt.Fprintf(w, "  Avg score:   %.3f\n", s.AvgScore)
	fmt.Fprintf(w, "  Avg latency: %.0fms\n", s.AvgLatencyMS)
	fmt.Fprintf(w, "  Score distribution:\n")
	for b := 0; b < 10; b++ {
		fmt.Fprintf(w, "    %.1f-%.1f %s %d\n", float64(b)/10, float64(b+1)/10, bar(s.Distribution[b]), s.Distribution[b])
	}
}

func critLabel(c results.CriterionResult) string {
	if c.Instructions != "" {
		return c.Instructions
	}
	return c.Name
}

func bar(n int) string {
	if n > 40 {
		n = 40
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = '#'
	}
	return string(b)
}
