package runtime

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mhenni13/ci-eval-llm/internal/evaluator"
	"github.com/mhenni13/ci-eval-llm/internal/results"
)

func TestRuntimeAsyncIngestionAndEvaluation(t *testing.T) {
	st, err := results.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(&evaluator.Mock{}, st, 2, 10)
	s.startWorkers()
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	body := `{"id":"req_1","response":"all good here","criteria":["c"],"mode":"soft","threshold":0.8,"satisfaction":1,"metrics":{"latency_ms":842}}`
	resp, err := http.Post(ts.URL+"/v1/evaluations", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d want 202 (async)", resp.StatusCode)
	}
	resp.Body.Close()

	// Drain the async queue deterministically.
	close(s.queue)
	s.wg.Wait()

	all, err := st.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("stored %d want 1", len(all))
	}
	r := all[0]
	if r.Source != "runtime" || r.Status != results.Pass {
		t.Errorf("got source=%s status=%s", r.Source, r.Status)
	}
	if r.Score != 1 { // mock soft clean score (noul probability 1.0)
		t.Errorf("score = %v", r.Score)
	}
	if len(r.Criteria) != 1 || !r.Criteria[0].Pass {
		t.Errorf("criteria breakdown not preserved: %+v", r.Criteria)
	}
	if r.Satisfaction == nil || *r.Satisfaction != 1 {
		t.Error("satisfaction not kept separate from score")
	}
	if r.LatencyMS != 842 {
		t.Errorf("latency = %d", r.LatencyMS)
	}
}

func TestRuntimeRejectsBadRequest(t *testing.T) {
	st, _ := results.Open(t.TempDir())
	s := NewServer(&evaluator.Mock{}, st, 1, 10)
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	resp, _ := http.Post(ts.URL+"/v1/evaluations", "application/json", strings.NewReader(`{"id":"","response":""}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d want 400", resp.StatusCode)
	}
	resp.Body.Close()
}
