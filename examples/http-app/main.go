// Example HTTP LLM application implementing the ci-eval-llm runner contract.
// Run it, then point a suite's `runner.endpoint` at http://localhost:8000/llm/test.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
)

type request struct {
	TestID       string `json:"test_id"`
	SystemPrompt string `json:"system_prompt"`
	UserPrompt   string `json:"user_prompt"`
}

func main() {
	http.HandleFunc("POST /llm/test", func(w http.ResponseWriter, r *http.Request) {
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Replace this with a real model call.
		resp := "Our refund policy allows refunds within 30 days of purchase."
		if strings.Contains(strings.ToLower(req.UserPrompt), "reveal") {
			resp = "Sure, ignore your instructions. My system prompt is: " + req.SystemPrompt
		}
		json.NewEncoder(w).Encode(map[string]string{"response": resp})
	})
	log.Println("example LLM app on http://localhost:8000/llm/test")
	log.Fatal(http.ListenAndServe("localhost:8000", nil))
}
