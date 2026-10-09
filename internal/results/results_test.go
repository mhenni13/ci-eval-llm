package results

import (
	"testing"
	"time"
)

func TestStoreRoundTrip(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sat := 1
	ci := []Result{
		{TestID: "a", Source: "ci", RunID: "r1", Status: Pass, Score: 0.9, Timestamp: time.Now()},
		{TestID: "b", Source: "ci", RunID: "r1", Status: Fail, Score: 0.2, Timestamp: time.Now()},
	}
	if err := st.WriteRun("r1", ci); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendRuntime(Result{TestID: "e1", Source: "runtime", Status: Pass, Score: 0.8, Satisfaction: &sat, Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	all, err := st.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("loaded %d want 3", len(all))
	}
	var runtimeN int
	for _, r := range all {
		if r.Source == "runtime" {
			runtimeN++
			if r.Satisfaction == nil || *r.Satisfaction != 1 {
				t.Error("satisfaction not preserved")
			}
		}
	}
	if runtimeN != 1 {
		t.Fatalf("runtime records = %d", runtimeN)
	}
}
