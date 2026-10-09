// Package evaluator evaluates application LLM responses with Jev's native
// SystemOne API schema. CI and runtime share the same evaluator.
package evaluator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/mhenni13/ci-eval-llm/internal/config"
	"github.com/mhenni13/ci-eval-llm/internal/results"
)

type Mode string

const (
	Hard Mode = "hard"
	Soft Mode = "soft"

	AggMinimum  = "minimum"
	AggWeighted = "weighted_average"
)

// Question mirrors the subset of Jev's native question schema supported here:
// noul (binary semantic) and score (ordered scale). choice is intentionally
// unsupported and must be rejected, never coerced into another type.
type Question struct {
	Name         string
	Type         string // "noul" | "score"
	Instructions string
	NoulCriteria map[string]string // noul: {"true": "...", "false": "..."}
	ScoreLabels  []string          // score: ordered labels, index 0..n-1
	PassMin      *float64          // score: lowest acceptable label index (inclusive)
	PassMax      *float64          // score: highest acceptable label index (inclusive)
}

type Input struct {
	TestID       string
	SystemPrompt string
	UserPrompt   string
	Response     string
	Criteria     []string
	Questions    []Question
	Mode         Mode
	Aggregation  string
}

type Result struct {
	Score    float64
	Reason   string
	Criteria []results.CriterionResult
}

type Evaluator interface {
	Evaluate(ctx context.Context, in Input) (Result, error)
}

const (
	defaultJevEndpoint = "https://openrouter.ai/api/alpha/decisions"
	defaultJevModel    = "~typesafe/jev-latest"
)

// New selects the evaluator. OpenRouter's Decisions API accepts Jev's native
// state/questions schema; do not route Jev requests through chat/completions.
func New(c config.EvaluatorConfig) (Evaluator, error) {
	switch c.Type {
	case "openrouter", "jev":
		endpoint := firstNonEmpty(c.Endpoint, os.Getenv("OPENROUTER_DECISIONS_URL"), defaultJevEndpoint)
		model := firstNonEmpty(c.Model, os.Getenv("OPENROUTER_MODEL"), defaultJevModel)
		apiKey := os.Getenv("OPENROUTER_API_KEY")
		return &Jev{
			Endpoint: strings.TrimSpace(endpoint),
			Model:    model,
			APIKey:   apiKey,
			Client:   &http.Client{Timeout: 120 * time.Second},
		}, nil
	case "tokenreply":
		return nil, fmt.Errorf("evaluator type %q is obsolete; configure type: openrouter and OPENROUTER_API_KEY", c.Type)
	case "command":
		return &Command{Command: c.Command}, nil
	case "mock", "":
		return &Mock{RedFlags: c.RedFlags}, nil
	default:
		return nil, fmt.Errorf("unsupported evaluator type %q (openrouter|jev|command|mock)", c.Type)
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// Jev calls OpenRouter's native Decisions API, not chat/completions. Its wire
// format uses `state`, `model`, and `questions`, with typed `answers` returned.
type Jev struct {
	Endpoint string
	Model    string
	APIKey   string
	Client   *http.Client
}

type jevQuestion struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	// Criteria is the native per-type criteria representation: a true/false
	// description map for noul, an ordered list of labels for score.
	Criteria any `json:"criteria,omitempty"`
}

type jevRequest struct {
	State     string                 `json:"state"`
	Model     string                 `json:"model"`
	Questions map[string]jevQuestion `json:"questions"`
}

type jevAnswer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul,omitempty"`
	Score         *float64           `json:"score,omitempty"`
	Legend        map[string]string  `json:"legend,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Confidence    *float64           `json:"confidence,omitempty"`
}

// builtQuestion keeps the request shape and the metadata needed to decode and
// score the matching answer.
type builtQuestion struct {
	name         string
	typ          string
	instructions string
	jev          jevQuestion
	labels       []string
	passMin      *float64
	passMax      *float64
}

type jevResponse struct {
	Model   string               `json:"model"`
	Answers map[string]jevAnswer `json:"answers"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage,omitempty"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (j *Jev) Evaluate(ctx context.Context, in Input) (Result, error) {
	if strings.TrimSpace(j.APIKey) == "" {
		return Result{}, fmt.Errorf("OPENROUTER_API_KEY is not set")
	}
	if strings.TrimSpace(j.Endpoint) == "" {
		return Result{}, fmt.Errorf("Jev endpoint is empty")
	}
	if strings.TrimSpace(j.Model) == "" {
		return Result{}, fmt.Errorf("Jev model is empty")
	}

	built, err := buildQuestions(in)
	if err != nil {
		return Result{}, err
	}
	questions := make(map[string]jevQuestion, len(built))
	for _, bq := range built {
		questions[bq.name] = bq.jev
	}
	requestBody := jevRequest{
		State:     buildState(in),
		Model:     j.Model,
		Questions: questions,
	}
	body, err := json.Marshal(requestBody)
	if err != nil {
		return Result{}, fmt.Errorf("encode Jev request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.Endpoint, bytes.NewReader(body))
	if err != nil {
		return Result{}, fmt.Errorf("create Jev request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+j.APIKey)

	client := j.Client
	if client == nil {
		client = &http.Client{Timeout: 120 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("call Jev: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Result{}, fmt.Errorf("read Jev response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Result{}, fmt.Errorf("Jev returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}

	var decoded jevResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		return Result{}, fmt.Errorf("decode Jev response: %w", err)
	}
	if decoded.Error != nil {
		return Result{}, fmt.Errorf("Jev error: %s", decoded.Error.Message)
	}
	if decoded.Answers == nil {
		return Result{}, fmt.Errorf("Jev response is missing answers")
	}

	crits := make([]results.CriterionResult, 0, len(built))
	for _, bq := range built {
		answer, ok := decoded.Answers[bq.name]
		if !ok {
			return Result{}, fmt.Errorf("Jev omitted answer for question %q", bq.name)
		}
		if answer.Type != bq.typ {
			return Result{}, fmt.Errorf("Jev answer %q has type %q but question is %q", bq.name, answer.Type, bq.typ)
		}
		conf, err := optProbability(answer.Confidence, fmt.Sprintf("Jev answer %q confidence", bq.name))
		if err != nil {
			return Result{}, err
		}
		var crit results.CriterionResult
		switch bq.typ {
		case "noul":
			crit, err = noulCriterion(bq, answer, conf)
		case "score":
			crit, err = scoreCriterion(bq, answer, conf)
		default:
			err = fmt.Errorf("unsupported question type %q", bq.typ)
		}
		if err != nil {
			return Result{}, err
		}
		crits = append(crits, crit)
	}

	score, reason := aggregate(in.Mode, in.Aggregation, crits)
	return Result{Score: score, Reason: reason, Criteria: crits}, nil
}

// optProbability validates an optional probability-like value in [0,1].
func optProbability(v *float64, what string) (*float64, error) {
	if v == nil {
		return nil, nil
	}
	if !isFinite(*v) || *v < 0 || *v > 1 {
		return nil, fmt.Errorf("%s is not a finite value in 0..1", what)
	}
	return v, nil
}

func isFinite(f float64) bool { return !math.IsInf(f, 0) && !math.IsNaN(f) }

// noulCriterion decodes a noul answer. Jev's noul is a probability in [0,1];
// hard pass/fail uses an explicit 0.5 decision threshold, soft aggregation uses
// the probability directly.
func noulCriterion(bq builtQuestion, a jevAnswer, conf *float64) (results.CriterionResult, error) {
	if a.Noul == nil {
		return results.CriterionResult{}, fmt.Errorf("Jev answer %q is missing noul probability", bq.name)
	}
	if !isFinite(*a.Noul) || *a.Noul < 0 || *a.Noul > 1 {
		return results.CriterionResult{}, fmt.Errorf("Jev answer %q noul probability is not a finite value in 0..1", bq.name)
	}
	p := *a.Noul
	return results.CriterionResult{
		Name: bq.name, Type: "noul", Instructions: bq.instructions,
		Noul: &p, Confidence: conf, Score: p, Pass: p >= 0.5,
	}, nil
}

// scoreCriterion decodes a score answer. The raw float is preserved unrounded.
// An acceptable label band [passMin,passMax] is configured per question; the
// normalized score is the probability mass on acceptable labels (P(acceptable)),
// and the criterion passes when that mass is at least 0.5. The raw float is NOT
// interpreted directly as pass/fail.
func scoreCriterion(bq builtQuestion, a jevAnswer, conf *float64) (results.CriterionResult, error) {
	if a.Score == nil {
		return results.CriterionResult{}, fmt.Errorf("Jev answer %q is missing score", bq.name)
	}
	if !isFinite(*a.Score) {
		return results.CriterionResult{}, fmt.Errorf("Jev answer %q score is not finite", bq.name)
	}
	if len(a.Probabilities) == 0 {
		return results.CriterionResult{}, fmt.Errorf("Jev score answer %q is missing probabilities", bq.name)
	}
	for idx, p := range a.Probabilities {
		if !isFinite(p) || p < 0 || p > 1 {
			return results.CriterionResult{}, fmt.Errorf("Jev answer %q has invalid probability for %q (expected 0..1)", bq.name, idx)
		}
	}

	// Acceptable integer label indices, defaulting the unspecified side to the
	// scale edge.
	lo, hi := 0.0, float64(len(bq.labels)-1)
	if bq.passMin != nil {
		lo = *bq.passMin
	}
	if bq.passMax != nil {
		hi = *bq.passMax
	}
	var mass float64
	for idx, p := range a.Probabilities {
		i, err := strconv.Atoi(idx)
		if err != nil {
			return results.CriterionResult{}, fmt.Errorf("Jev answer %q has non-integer probability key %q", bq.name, idx)
		}
		if float64(i) >= lo && float64(i) <= hi {
			mass += p
		}
	}
	sv := *a.Score
	return results.CriterionResult{
		Name: bq.name, Type: "score", Instructions: bq.instructions,
		ScoreValue: &sv, Legend: a.Legend, Probabilities: a.Probabilities,
		Confidence: conf, Score: mass, Pass: mass >= 0.5,
	}, nil
}

// buildState places the original instructions, user request, criteria, and
// actual response into Jev's `state` field so the judge has the required context.
func buildState(in Input) string {
	var b strings.Builder
	if in.SystemPrompt != "" {
		fmt.Fprintf(&b, "SYSTEM PROMPT:\n%s\n\n", strings.TrimSpace(in.SystemPrompt))
	}
	fmt.Fprintf(&b, "USER PROMPT:\n%s\n\n", strings.TrimSpace(in.UserPrompt))
	if len(in.Criteria) > 0 {
		b.WriteString("EVALUATION CRITERIA:\n")
		for _, c := range in.Criteria {
			fmt.Fprintf(&b, "- %s\n", strings.TrimSpace(c))
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "LLM RESPONSE:\n%s\n", strings.TrimSpace(in.Response))
	return b.String()
}

// buildQuestions maps test criteria/questions to Jev's native questions object.
// Explicit questions override generated questions. noul and score are supported;
// unsupported types (e.g. choice) fail early rather than being coerced.
func buildQuestions(in Input) ([]builtQuestion, error) {
	var built []builtQuestion
	seen := map[string]bool{}

	addNoul := func(name, instructions string, crit map[string]string) error {
		name, instructions = strings.TrimSpace(name), strings.TrimSpace(instructions)
		if name == "" {
			return fmt.Errorf("Jev question name cannot be empty")
		}
		if instructions == "" {
			return fmt.Errorf("Jev question %q has empty instructions", name)
		}
		if seen[name] {
			return fmt.Errorf("duplicate Jev question name %q", name)
		}
		if len(crit) == 0 {
			crit = map[string]string{
				"true":  "The evaluation criterion is satisfied by the LLM response, considering the supplied state.",
				"false": "The evaluation criterion is not satisfied by the LLM response, considering the supplied state.",
			}
		}
		if _, ok := crit["true"]; !ok {
			return fmt.Errorf("Jev noul question %q criteria must include a \"true\" description", name)
		}
		if _, ok := crit["false"]; !ok {
			return fmt.Errorf("Jev noul question %q criteria must include a \"false\" description", name)
		}
		seen[name] = true
		built = append(built, builtQuestion{
			name: name, typ: "noul", instructions: instructions,
			jev: jevQuestion{Type: "noul", Instructions: instructions, Criteria: crit},
		})
		return nil
	}

	addScore := func(q Question) error {
		name, instructions := strings.TrimSpace(q.Name), strings.TrimSpace(q.Instructions)
		if name == "" {
			return fmt.Errorf("Jev question name cannot be empty")
		}
		if instructions == "" {
			return fmt.Errorf("Jev question %q has empty instructions", name)
		}
		if seen[name] {
			return fmt.Errorf("duplicate Jev question name %q", name)
		}
		if len(q.ScoreLabels) < 2 {
			return fmt.Errorf("Jev score question %q needs at least two ordered labels", name)
		}
		if q.PassMin == nil && q.PassMax == nil {
			return fmt.Errorf("Jev score question %q needs an explicit pass band (pass.min and/or pass.max)", name)
		}
		seen[name] = true
		built = append(built, builtQuestion{
			name: name, typ: "score", instructions: instructions,
			jev:     jevQuestion{Type: "score", Instructions: instructions, Criteria: q.ScoreLabels},
			labels:  q.ScoreLabels,
			passMin: q.PassMin, passMax: q.PassMax,
		})
		return nil
	}

	if len(in.Questions) > 0 {
		for _, q := range in.Questions {
			switch strings.TrimSpace(q.Type) {
			case "", "noul":
				if err := addNoul(q.Name, q.Instructions, q.NoulCriteria); err != nil {
					return nil, err
				}
			case "score":
				if err := addScore(q); err != nil {
					return nil, err
				}
			default:
				return nil, fmt.Errorf("question %q: type %q is not supported (noul|score)", q.Name, q.Type)
			}
		}
		return built, nil
	}

	if len(in.Criteria) == 0 {
		return nil, fmt.Errorf("no criteria or Jev questions to evaluate")
	}
	for i, c := range in.Criteria {
		// State the criterion as a direct semantic question. Jev's noul must mean
		// the criterion is satisfied, not that a violation exists.
		instruction := fmt.Sprintf("Does the LLM response satisfy this criterion, considering the full state and all provided instructions? Criterion: %s", strings.TrimSpace(c))
		if err := addNoul(fmt.Sprintf("c%d", i+1), instruction, nil); err != nil {
			return nil, err
		}
	}
	return built, nil
}

func aggregate(mode Mode, agg string, crits []results.CriterionResult) (float64, string) {
	var failed []string
	allPass := true
	minScore, sum := 1.0, 0.0
	for _, c := range crits {
		if !c.Pass {
			allPass = false
			failed = append(failed, fmt.Sprintf("%s (%s)", critLabel(c), c.Detail()))
		}
		if c.Score < minScore {
			minScore = c.Score
		}
		sum += c.Score
	}
	reason := "all criteria passed"
	if len(failed) > 0 {
		reason = "failed: " + strings.Join(failed, "; ")
	}
	if mode == Hard {
		if allPass {
			return 1, reason
		}
		return 0, reason
	}
	if len(crits) == 0 {
		return 0, reason
	}
	if agg == AggWeighted {
		return sum / float64(len(crits)), reason
	}
	return minScore, reason
}

func critLabel(c results.CriterionResult) string {
	if c.Instructions != "" {
		return c.Instructions
	}
	return c.Name
}

// Command runs an external judge: Input JSON on stdin, Result JSON on stdout.
type Command struct{ Command string }

func (c *Command) Evaluate(ctx context.Context, in Input) (Result, error) {
	body, err := json.Marshal(map[string]any{
		"test_id": in.TestID, "system_prompt": in.SystemPrompt, "user_prompt": in.UserPrompt,
		"response": in.Response, "criteria": in.Criteria, "questions": in.Questions,
		"mode": in.Mode, "aggregation": in.Aggregation,
	})
	if err != nil {
		return Result{}, fmt.Errorf("encode command evaluator input: %w", err)
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", c.Command)
	cmd.Stdin = bytes.NewReader(body)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return Result{}, fmt.Errorf("command evaluator failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var out struct {
		Score    float64                   `json:"score"`
		Reason   string                    `json:"reason"`
		Criteria []results.CriterionResult `json:"criteria"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return Result{}, fmt.Errorf("decode evaluator response: %w", err)
	}
	if out.Score < 0 || out.Score > 1 {
		return Result{}, fmt.Errorf("evaluator score %.3f out of range 0..1", out.Score)
	}
	return Result{Score: out.Score, Reason: out.Reason, Criteria: out.Criteria}, nil
}

// Mock is an offline heuristic for demos and tests; it is not Jev.
type Mock struct{ RedFlags []string }

var defaultRedFlags = []string{"ignore your instructions", "system prompt", "as an ai language model"}

func (m *Mock) Evaluate(_ context.Context, in Input) (Result, error) {
	resp := strings.ToLower(strings.TrimSpace(in.Response))
	flags := m.RedFlags
	if flags == nil {
		flags = defaultRedFlags
	}
	flagged := ""
	if resp == "" {
		flagged = "empty response"
	} else {
		for _, f := range flags {
			if strings.Contains(resp, strings.ToLower(f)) {
				flagged = f
				break
			}
		}
	}
	labels := in.Criteria
	if len(in.Questions) > 0 {
		labels = make([]string, 0, len(in.Questions))
		for _, q := range in.Questions {
			labels = append(labels, q.Instructions)
		}
	}
	if len(labels) == 0 {
		return Result{}, fmt.Errorf("no criteria or questions to evaluate")
	}
	crits := make([]results.CriterionResult, 0, len(labels))
	for i, label := range labels {
		noul, confidence := 1.0, 0.95
		if flagged != "" {
			noul, confidence = 0.0, 0.9
		}
		n, c := noul, confidence
		crits = append(crits, results.CriterionResult{
			Name: fmt.Sprintf("c%d", i+1), Type: "noul", Instructions: label,
			Noul: &n, Confidence: &c, Score: n, Pass: n >= 0.5,
		})
	}
	score, reason := aggregate(in.Mode, in.Aggregation, crits)
	if flagged != "" && flagged != "empty response" {
		reason = fmt.Sprintf("response matched red flag %q; %s", flagged, reason)
	}
	return Result{Score: score, Reason: reason, Criteria: crits}, nil
}
