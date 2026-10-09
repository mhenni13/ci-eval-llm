package report

import (
	"strings"
	"testing"

	"github.com/mhenni13/ci-eval-llm/internal/results"
)

func TestBuildAggregates(t *testing.T) {
	sat0, sat1 := 0, 1
	rs := []results.Result{
		{Source: "ci", RunID: "r1", Status: results.Pass, Score: 0.9, LatencyMS: 100},
		{Source: "ci", RunID: "r1", Status: results.Fail, Score: 0.2, LatencyMS: 200, TestID: "f"},
		{Source: "ci", RunID: "r2", Status: results.Error, LatencyMS: 50, TestID: "e", Error: "boom"},
		{Source: "runtime", Status: results.Pass, Score: 0.8, Satisfaction: &sat1},
		{Source: "runtime", Status: results.Fail, Score: 0.3, Satisfaction: &sat0},
	}

	ci := Build(rs, "ci")
	if ci.Total != 3 || ci.Passed != 1 || ci.Failed != 1 || ci.Errors != 1 {
		t.Fatalf("ci summary = %+v", ci)
	}
	if ci.PassRate != 50 { // 1 pass of 2 evaluated
		t.Errorf("pass rate = %v", ci.PassRate)
	}
	if RunCount(rs) != 2 {
		t.Errorf("run count = %d", RunCount(rs))
	}

	rt := Build(rs, "runtime")
	if rt.Satisfaction == nil || *rt.Satisfaction != 0.5 {
		t.Errorf("satisfaction = %v", rt.Satisfaction)
	}

	var b strings.Builder
	WriteText(&b, rs)
	out := b.String()
	if !strings.Contains(out, "User satisfaction") || !strings.Contains(out, "ERROR") {
		t.Errorf("report text missing sections:\n%s", out)
	}
}
