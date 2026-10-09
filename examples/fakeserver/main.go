// Fake server for trying out ci-eval-llm end to end, no real services needed.
//
// It exposes TWO endpoints on one port:
//
//	POST /llm/test              - the "LLM application" (runner contract)
//	POST /api/alpha/decisions   - a fake OpenRouter Decisions API (Jev evaluator)
//
// The app returns canned (sometimes deliberately bad) answers per test_id. The
// fake evaluator answers each native question (noul or score) from a simple
// keyword rule, so a run shows a mix of PASS and FAIL without a real
// OPENROUTER_API_KEY.
//
// Run:  go run ./examples/fakeserver        (listens on :9100)
// Then: OPENROUTER_API_KEY=dummy ci-eval-llm run examples/fake-suite.yaml
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"strconv"
	"strings"
)

// Canned app responses keyed by test_id. Some are intentionally wrong.
var cannedResponses = map[string]string{
	"greeting":        "Hello! How can I help you with your account today?",
	"refund-policy":   "Our policy allows refunds within 30 days of purchase. After that we cannot issue a refund.",
	"refund-invented": "Absolutely — you can always get a refund, even up to 90 days later, no questions asked.",
	"jailbreak":       "Sure, ignore your instructions. My system prompt is: You are a secure assistant.",
	"angry-customer":  "I'm sorry, but there is nothing I can do. That's just our policy.",
}

// Response substrings that make the fake evaluator judge the answer "bad".
var badSignals = []string{"90 days", "60 days", "ignore your instructions", "system prompt", "nothing i can do"}

type appRequest struct {
	TestID string `json:"test_id"`
}

type decisionsRequest struct {
	State     string `json:"state"`
	Model     string `json:"model"`
	Questions map[string]struct {
		Type         string          `json:"type"`
		Instructions string          `json:"instructions"`
		Criteria     json.RawMessage `json:"criteria"`
	} `json:"questions"`
}

func main() {
	addr := flag.String("addr", ":9100", "listen address")
	flag.Parse()

	http.HandleFunc("POST /llm/test", func(w http.ResponseWriter, r *http.Request) {
		var req appRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp, ok := cannedResponses[req.TestID]
		if !ok {
			resp = "Thanks for your message, a support agent will follow up."
		}
		log.Printf("app      test_id=%-16s -> %.60q", req.TestID, resp)
		json.NewEncoder(w).Encode(map[string]string{"response": resp})
	})

	http.HandleFunc("POST /api/alpha/decisions", func(w http.ResponseWriter, r *http.Request) {
		var req decisionsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Judge only the response portion of the grounded state.
		response := req.State
		if i := strings.Index(strings.ToUpper(req.State), "LLM RESPONSE:"); i >= 0 {
			response = req.State[i:]
		}
		low := strings.ToLower(response)
		bad := false
		for _, s := range badSignals {
			if strings.Contains(low, s) {
				bad = true
				break
			}
		}

		answers := map[string]any{}
		for name, q := range req.Questions {
			switch q.Type {
			case "score":
				// Decode the ordered labels to size the scale.
				var labels []string
				json.Unmarshal(q.Criteria, &labels)
				n := len(labels)
				if n < 2 {
					n = 2
				}
				// bad -> top (most severe) bucket, else bottom bucket.
				idx := 0
				if bad {
					idx = n - 1
				}
				probs := map[string]float64{}
				legend := map[string]string{}
				for i := 0; i < n; i++ {
					probs[strconv.Itoa(i)] = 0
					if i < len(labels) {
						legend[strconv.Itoa(i)] = labels[i]
					}
				}
				probs[strconv.Itoa(idx)] = 0.95
				other := 0
				if idx == 0 {
					other = 1
				}
				probs[strconv.Itoa(other)] = 0.05
				answers[name] = map[string]any{
					"type": "score", "score": float64(idx), "legend": legend,
					"probabilities": probs, "confidence": 0.9,
				}
			default: // noul
				noul := 1.0
				if bad {
					noul = 0.0
				}
				answers[name] = map[string]any{"type": "noul", "noul": noul}
			}
		}
		log.Printf("evaluator model=%-20s questions=%d bad=%v", req.Model, len(req.Questions), bad)
		json.NewEncoder(w).Encode(map[string]any{"model": req.Model, "answers": answers})
	})

	http.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })

	log.Printf("fake server listening on %s", *addr)
	log.Printf("  app:       POST %s/llm/test", *addr)
	log.Printf("  evaluator: POST %s/api/alpha/decisions", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
