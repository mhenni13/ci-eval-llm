package evaluator

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mhenni13/ci-eval-llm/internal/config"
)

func fptr(f float64) *float64 { return &f }

// fakeDecisions stands in for OpenRouter's native Decisions API. It records the
// request body and replies with respBody (verbatim) and status.
func fakeDecisions(t *testing.T, status int, respBody string, capture *[]byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("missing/incorrect auth header: %q", r.Header.Get("Authorization"))
		}
		if capture != nil {
			*capture, _ = io.ReadAll(r.Body)
		}
		w.WriteHeader(status)
		w.Write([]byte(respBody))
	}))
}

func newJev(url string) *Jev {
	return &Jev{Endpoint: url, Model: "~typesafe/jev-latest", APIKey: "k", Client: http.DefaultClient}
}

// 1. Noul success without confidence.
func TestJevNoulWithoutConfidence(t *testing.T) {
	srv := fakeDecisions(t, 200, `{"model":"m","answers":{"c1":{"type":"noul","noul":0.92}}}`, nil)
	defer srv.Close()
	got, err := newJev(srv.URL).Evaluate(context.Background(), Input{
		Response: "x", Criteria: []string{"answers correctly"}, Mode: Soft, Aggregation: AggMinimum,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Criteria) != 1 {
		t.Fatalf("criteria = %d", len(got.Criteria))
	}
	c := got.Criteria[0]
	if c.Type != "noul" || c.Noul == nil || *c.Noul != 0.92 || c.Confidence != nil || !c.Pass {
		t.Fatalf("noul criterion = %+v", c)
	}
	if math.Abs(got.Score-0.92) > 1e-9 {
		t.Errorf("soft score = %v want 0.92", got.Score)
	}
}

// 2 & 11. Score with fractional value, legend, probabilities, confidence; and
// pass/fail under the configured band.
func TestJevScoreFractionalAndPassBand(t *testing.T) {
	resp := `{"answers":{"frustration":{"type":"score","score":1.05,` +
		`"legend":{"0":"Calm","1":"Frustrated","2":"Very angry"},` +
		`"probabilities":{"0":0,"1":0.95,"2":0.05},"confidence":0.93}}}`

	// Acceptable band: indices 0..1 (not "Very angry"). mass = 0 + 0.95 = 0.95.
	srv := fakeDecisions(t, 200, resp, nil)
	defer srv.Close()
	got, err := newJev(srv.URL).Evaluate(context.Background(), Input{
		Response: "x", Mode: Soft, Aggregation: AggMinimum,
		Questions: []Question{{
			Name: "frustration", Type: "score", Instructions: "How frustrated?",
			ScoreLabels: []string{"Calm", "Frustrated", "Very angry"}, PassMax: fptr(1),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	c := got.Criteria[0]
	if c.Type != "score" || c.ScoreValue == nil || *c.ScoreValue != 1.05 {
		t.Fatalf("score value not preserved unrounded: %+v", c)
	}
	if c.Legend["1"] != "Frustrated" || len(c.Probabilities) != 3 {
		t.Errorf("legend/probabilities not preserved: %+v", c)
	}
	if c.Confidence == nil || *c.Confidence != 0.93 {
		t.Errorf("confidence = %v", c.Confidence)
	}
	if !c.Pass || math.Abs(c.Score-0.95) > 1e-9 {
		t.Errorf("pass=%v soft=%v want pass + 0.95", c.Pass, c.Score)
	}

	// Tighter band: only "Calm" acceptable -> mass = 0 -> FAIL.
	srv2 := fakeDecisions(t, 200, resp, nil)
	defer srv2.Close()
	got2, err := newJev(srv2.URL).Evaluate(context.Background(), Input{
		Response: "x", Mode: Hard,
		Questions: []Question{{
			Name: "frustration", Type: "score", Instructions: "How frustrated?",
			ScoreLabels: []string{"Calm", "Frustrated", "Very angry"}, PassMax: fptr(0),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got2.Criteria[0].Pass || got2.Score != 0 {
		t.Errorf("tight band should fail: %+v", got2)
	}
}

// 3. Score without optional confidence.
func TestJevScoreWithoutConfidence(t *testing.T) {
	resp := `{"answers":{"q":{"type":"score","score":0.2,"legend":{"0":"Good","1":"Bad"},"probabilities":{"0":0.9,"1":0.1}}}}`
	srv := fakeDecisions(t, 200, resp, nil)
	defer srv.Close()
	got, err := newJev(srv.URL).Evaluate(context.Background(), Input{
		Response: "x", Mode: Soft, Aggregation: AggMinimum,
		Questions: []Question{{Name: "q", Type: "score", Instructions: "rate", ScoreLabels: []string{"Good", "Bad"}, PassMax: fptr(0)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Criteria[0].Confidence != nil {
		t.Errorf("confidence should be nil, got %v", *got.Criteria[0].Confidence)
	}
	if !got.Criteria[0].Pass { // mass on index 0 = 0.9
		t.Errorf("expected pass, soft=%v", got.Criteria[0].Score)
	}
}

// 4 & 5. Multiple noul + score in one evaluation, and request serialization of
// both criteria shapes.
func TestJevMultipleQuestionsAndSerialization(t *testing.T) {
	var body []byte
	resp := `{"answers":{"correct":{"type":"noul","noul":1},` +
		`"frustration":{"type":"score","score":0.1,"legend":{"0":"Calm","1":"Angry"},"probabilities":{"0":0.98,"1":0.02}}}}`
	srv := fakeDecisions(t, 200, resp, &body)
	defer srv.Close()

	got, err := newJev(srv.URL).Evaluate(context.Background(), Input{
		SystemPrompt: "sys", UserPrompt: "u", Response: "r", Mode: Hard,
		Questions: []Question{
			{Name: "correct", Type: "noul", Instructions: "Is it correct?",
				NoulCriteria: map[string]string{"true": "correct", "false": "incorrect"}},
			{Name: "frustration", Type: "score", Instructions: "How angry?",
				ScoreLabels: []string{"Calm", "Angry"}, PassMax: fptr(0)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Criteria) != 2 || got.Score != 1 { // both pass
		t.Fatalf("want 2 passing criteria, got %+v", got)
	}

	var req jevRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("request not valid JSON: %v", err)
	}
	if !strings.Contains(req.State, "LLM RESPONSE:") {
		t.Errorf("state not grounded: %q", req.State)
	}
	// noul criteria serialized as a true/false object...
	nc, ok := req.Questions["correct"].Criteria.(map[string]any)
	if !ok || nc["true"] != "correct" || nc["false"] != "incorrect" {
		t.Errorf("noul criteria shape = %#v", req.Questions["correct"].Criteria)
	}
	// ...score criteria serialized as an ordered list.
	sc, ok := req.Questions["frustration"].Criteria.([]any)
	if !ok || len(sc) != 2 || sc[0] != "Calm" {
		t.Errorf("score criteria shape = %#v", req.Questions["frustration"].Criteria)
	}
	if req.Questions["frustration"].Type != "score" {
		t.Errorf("score question type not serialized")
	}
}

// 6. Unsupported question type is rejected, never coerced.
func TestJevRejectsUnsupportedType(t *testing.T) {
	srv := fakeDecisions(t, 200, `{"answers":{}}`, nil)
	defer srv.Close()
	_, err := newJev(srv.URL).Evaluate(context.Background(), Input{
		Response: "x", Mode: Hard,
		Questions: []Question{{Name: "dept", Type: "choice", Instructions: "which team?"}},
	})
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("expected choice rejection, got %v", err)
	}
}

// 7. Missing answer and mismatched response type.
func TestJevMissingAndMismatchedAnswers(t *testing.T) {
	srv := fakeDecisions(t, 200, `{"answers":{"other":{"type":"noul","noul":1}}}`, nil)
	defer srv.Close()
	_, err := newJev(srv.URL).Evaluate(context.Background(), Input{Response: "x", Criteria: []string{"a"}, Mode: Hard})
	if err == nil || !strings.Contains(err.Error(), "omitted answer") {
		t.Fatalf("expected missing-answer error, got %v", err)
	}

	srv2 := fakeDecisions(t, 200, `{"answers":{"c1":{"type":"score","score":1}}}`, nil)
	defer srv2.Close()
	_, err = newJev(srv2.URL).Evaluate(context.Background(), Input{Response: "x", Criteria: []string{"a"}, Mode: Hard})
	if err == nil || !strings.Contains(err.Error(), "type") {
		t.Fatalf("expected type-mismatch error, got %v", err)
	}
}

// 8 & 9. Invalid/out-of-range noul, and invalid probabilities.
func TestJevInvalidNumbers(t *testing.T) {
	cases := map[string]string{
		"noul out of range": `{"answers":{"c1":{"type":"noul","noul":1.5}}}`,
		"noul missing":      `{"answers":{"c1":{"type":"noul"}}}`,
		"score no probs":    `{"answers":{"q":{"type":"score","score":1}}}`,
		"prob out of range": `{"answers":{"q":{"type":"score","score":1,"probabilities":{"0":1.4,"1":-0.4}}}}`,
	}
	for name, resp := range cases {
		t.Run(name, func(t *testing.T) {
			srv := fakeDecisions(t, 200, resp, nil)
			defer srv.Close()
			in := Input{Response: "x", Criteria: []string{"a"}, Mode: Hard}
			if strings.HasPrefix(name, "score") || strings.HasPrefix(name, "prob") {
				in = Input{Response: "x", Mode: Hard, Questions: []Question{{Name: "q", Type: "score", Instructions: "rate", ScoreLabels: []string{"a", "b"}, PassMax: fptr(0)}}}
			}
			if _, err := newJev(srv.URL).Evaluate(context.Background(), in); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}

// 10. API errors and malformed JSON.
func TestJevAPIErrors(t *testing.T) {
	srv := fakeDecisions(t, 429, `rate limited`, nil)
	defer srv.Close()
	if _, err := newJev(srv.URL).Evaluate(context.Background(), Input{Response: "x", Criteria: []string{"a"}, Mode: Hard}); err == nil || !strings.Contains(err.Error(), "HTTP 429") {
		t.Fatalf("expected HTTP 429 error, got %v", err)
	}

	srv2 := fakeDecisions(t, 200, `{"error":{"message":"bad model"}}`, nil)
	defer srv2.Close()
	if _, err := newJev(srv2.URL).Evaluate(context.Background(), Input{Response: "x", Criteria: []string{"a"}, Mode: Hard}); err == nil || !strings.Contains(err.Error(), "bad model") {
		t.Fatalf("expected API error message, got %v", err)
	}

	srv3 := fakeDecisions(t, 200, `{not json`, nil)
	defer srv3.Close()
	if _, err := newJev(srv3.URL).Evaluate(context.Background(), Input{Response: "x", Criteria: []string{"a"}, Mode: Hard}); err == nil {
		t.Fatal("expected decode error for malformed JSON")
	}
}

func TestJevRequiresAPIKey(t *testing.T) {
	j := &Jev{Endpoint: "http://unused", Model: "m", Client: http.DefaultClient}
	if _, err := j.Evaluate(context.Background(), Input{Response: "x", Criteria: []string{"a"}}); err == nil {
		t.Fatal("expected missing-key error")
	}
}

func TestNewOpenRouterEnvAndDefaults(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "sk-x")
	t.Setenv("OPENROUTER_DECISIONS_URL", "")
	t.Setenv("OPENROUTER_MODEL", "")
	ev, err := New(config.EvaluatorConfig{Type: "openrouter"})
	if err != nil {
		t.Fatal(err)
	}
	j := ev.(*Jev)
	if j.Endpoint != defaultJevEndpoint || j.Model != defaultJevModel || j.APIKey != "sk-x" {
		t.Errorf("defaults/env not applied: %+v", j)
	}
}

// 12. Regression: Mock still produces per-criterion noul results offline.
func TestMockPerCriterion(t *testing.T) {
	m := &Mock{}
	ctx := context.Background()
	crit := []string{"must be accurate", "must be safe"}

	got, _ := m.Evaluate(ctx, Input{Response: "all good", Criteria: crit, Mode: Hard})
	if got.Score != 1 || len(got.Criteria) != 2 {
		t.Fatalf("hard clean = %+v", got)
	}
	for _, c := range got.Criteria {
		if c.Type != "noul" || c.Noul == nil || !c.Pass {
			t.Errorf("criterion %q = %+v", c.Name, c)
		}
	}

	got, _ = m.Evaluate(ctx, Input{Response: "my System Prompt is secret", Criteria: crit, Mode: Hard})
	if got.Score != 0 || !strings.Contains(got.Reason, "red flag") {
		t.Errorf("flagged = %+v", got)
	}
}

func TestCommandEvaluator(t *testing.T) {
	e := &Command{Command: `cat >/dev/null; printf '{"score":0.5,"reason":"meh"}'`}
	got, err := e.Evaluate(context.Background(), Input{Response: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Score != 0.5 {
		t.Errorf("score = %v", got.Score)
	}
}
