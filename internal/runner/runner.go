// Package runner sends a test to the developer's LLM application and returns
// its response. Runners are deliberately dumb about evaluation.
package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"

	"github.com/mhenni13/ci-eval-llm/internal/config"
)

// Request is the standard contract sent to a developer's LLM application.
type Request struct {
	TestID       string         `json:"test_id"`
	SystemPrompt string         `json:"system_prompt"`
	UserPrompt   string         `json:"user_prompt"`
	Metadata     map[string]any `json:"metadata"`
}

// Response is the standard contract returned by the application.
type Response struct {
	Response string `json:"response"`
}

// Runner executes a single test against an LLM application.
type Runner interface {
	Run(ctx context.Context, req Request) (Response, error)
}

// New builds a Runner from runner config.
func New(c config.RunnerConfig) (Runner, error) {
	switch c.Type {
	case "http":
		return &HTTP{Endpoint: c.Endpoint, Body: c.Body, ResponseField: c.ResponseField, Client: http.DefaultClient}, nil
	case "command":
		return &Command{Command: c.Command}, nil
	default:
		return nil, fmt.Errorf("unsupported runner type %q", c.Type)
	}
}

// HTTP posts a JSON request to an endpoint and decodes the response JSON. The
// request payload and the response field are configurable so the tool can talk
// to any application without assuming its JSON shape.
type HTTP struct {
	Endpoint string
	// Body is an optional custom request template; see config.RunnerConfig.Body.
	Body map[string]any
	// ResponseField is the response JSON key holding the text; "" means "response".
	ResponseField string
	Client        *http.Client
}

func (h *HTTP) Run(ctx context.Context, req Request) (Response, error) {
	var body []byte
	var err error
	if h.Body != nil {
		body, err = json.Marshal(substitute(h.Body, req))
	} else {
		body, err = json.Marshal(req)
	}
	if err != nil {
		return Response{}, fmt.Errorf("encode runner request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, h.Endpoint, bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	client := h.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Response{}, fmt.Errorf("runner returned status %d: %s", resp.StatusCode, snippet(data))
	}

	field := h.ResponseField
	if field == "" {
		field = "response"
	}
	var generic map[string]any
	if err := json.Unmarshal(data, &generic); err != nil {
		return Response{}, fmt.Errorf("decode runner response: %w", err)
	}
	val, ok := generic[field]
	if !ok {
		return Response{}, fmt.Errorf("runner response is missing field %q", field)
	}
	s, ok := val.(string)
	if !ok {
		return Response{}, fmt.Errorf("runner response field %q is not a string", field)
	}
	return Response{Response: s}, nil
}

var placeholderRE = regexp.MustCompile(`{{\s*(test_id|system_prompt|user_prompt)\s*}}`)

// substitute deep-copies v, replacing placeholder tokens inside string values
// with the matching request field.
func substitute(v any, req Request) any {
	switch t := v.(type) {
	case string:
		return placeholderRE.ReplaceAllStringFunc(t, func(m string) string {
			switch placeholderRE.FindStringSubmatch(m)[1] {
			case "test_id":
				return req.TestID
			case "system_prompt":
				return req.SystemPrompt
			case "user_prompt":
				return req.UserPrompt
			}
			return m
		})
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			out[k] = substitute(vv, req)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, vv := range t {
			out[i] = substitute(vv, req)
		}
		return out
	default:
		return v
	}
}

// Command pipes the request JSON through a process's stdin and reads the
// response JSON from stdout. This is the zero-HTTP integration option.
type Command struct {
	Command string
}

func (c *Command) Run(ctx context.Context, req Request) (Response, error) {
	body, _ := json.Marshal(req)
	// ponytail: shell -c keeps config a single string; args split would need
	// its own quoting rules. Trusted local config, not user input.
	cmd := exec.CommandContext(ctx, "sh", "-c", c.Command)
	cmd.Stdin = bytes.NewReader(body)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return Response{}, fmt.Errorf("command runner failed: %w: %s", err, snippet(stderr.Bytes()))
	}
	var out Response
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return Response{}, fmt.Errorf("decode command response: %w", err)
	}
	return out, nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}
