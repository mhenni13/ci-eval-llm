package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPRunner(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req Request
		json.NewDecoder(r.Body).Decode(&req)
		if req.TestID != "t1" {
			t.Errorf("test_id = %q", req.TestID)
		}
		json.NewEncoder(w).Encode(Response{Response: "echo:" + req.UserPrompt})
	}))
	defer srv.Close()

	r := &HTTP{Endpoint: srv.URL}
	resp, err := r.Run(context.Background(), Request{TestID: "t1", UserPrompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Response != "echo:hi" {
		t.Errorf("response = %q", resp.Response)
	}
}

func TestHTTPRunnerCustomBodyAndResponseField(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		// This server speaks {message} -> {reply}, not the default contract.
		msg, _ := got["message"].(string)
		if msg == "" {
			http.Error(w, "message field is required", http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"reply": "pong:" + msg})
	}))
	defer srv.Close()

	r := &HTTP{
		Endpoint:      srv.URL,
		Body:          map[string]any{"message": "{{user_prompt}}", "sys": "{{system_prompt}}", "nested": []any{map[string]any{"id": "{{test_id}}"}}},
		ResponseField: "reply",
	}
	resp, err := r.Run(context.Background(), Request{TestID: "t9", SystemPrompt: "be nice", UserPrompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Response != "pong:hello" {
		t.Errorf("response = %q", resp.Response)
	}
	// Placeholders substituted, including nested structures.
	if got["message"] != "hello" || got["sys"] != "be nice" {
		t.Errorf("top-level substitution wrong: %#v", got)
	}
	nested := got["nested"].([]any)[0].(map[string]any)
	if nested["id"] != "t9" {
		t.Errorf("nested substitution wrong: %#v", nested)
	}
}

func TestHTTPRunnerMissingResponseField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"unexpected": "x"})
	}))
	defer srv.Close()
	if _, err := (&HTTP{Endpoint: srv.URL}).Run(context.Background(), Request{UserPrompt: "u"}); err == nil {
		t.Fatal("expected missing-field error")
	}
}

func TestHTTPRunnerNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	if _, err := (&HTTP{Endpoint: srv.URL}).Run(context.Background(), Request{}); err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestCommandRunner(t *testing.T) {
	// Reads request JSON on stdin, echoes a fixed response.
	r := &Command{Command: `cat >/dev/null; printf '{"response":"ok"}'`}
	resp, err := r.Run(context.Background(), Request{TestID: "x", UserPrompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Response != "ok" {
		t.Errorf("response = %q", resp.Response)
	}
}

func TestCommandRunnerBadJSON(t *testing.T) {
	r := &Command{Command: `printf 'not json'`}
	if _, err := r.Run(context.Background(), Request{}); err == nil {
		t.Fatal("expected decode error")
	}
}
