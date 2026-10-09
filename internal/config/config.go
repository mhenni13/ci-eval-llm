// Package config parses and validates YAML test suites.
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Suite is a parsed, defaults-merged test suite.
type Suite struct {
	Name      string          `yaml:"-"`
	Runner    RunnerConfig    `yaml:"runner"`
	Evaluator EvaluatorConfig `yaml:"evaluator"`
	Tests     []Test          `yaml:"tests"`
}

type RunnerConfig struct {
	Type     string `yaml:"type"`     // http | command
	Endpoint string `yaml:"endpoint"` // http
	Command  string `yaml:"command"`  // command

	// Body is an optional custom request JSON payload for the http runner. Any
	// string value may contain the placeholders {{test_id}}, {{system_prompt}}
	// and {{user_prompt}}, which are substituted per test. When unset, the
	// default payload {test_id, system_prompt, user_prompt, metadata} is sent.
	Body map[string]any `yaml:"body"`
	// ResponseField is the JSON key in the http response holding the response
	// text. Defaults to "response".
	ResponseField string `yaml:"response_field"`
}

type EvaluatorConfig struct {
	Type     string   `yaml:"type"`     // openrouter | command | mock
	Endpoint string   `yaml:"endpoint"` // openrouter: Decisions API URL override
	Model    string   `yaml:"model"`    // openrouter: e.g. ~typesafe/jev-latest
	Command  string   `yaml:"command"`  // command
	RedFlags []string `yaml:"red_flags"`
}

// ScorePass is the acceptable label-index band for a score question (inclusive).
type ScorePass struct {
	Min *float64 `yaml:"min"`
	Max *float64 `yaml:"max"`
}

// Question is an explicit Jev question (advanced escape hatch). It supports Jev's
// noul and score question types. The `criteria` field is polymorphic: a
// true/false description map for noul, an ordered list of labels for score.
type Question struct {
	Type         string
	Instructions string
	NoulCriteria map[string]string // noul: {"true": "...", "false": "..."}
	ScoreLabels  []string          // score: ordered labels
	Pass         *ScorePass        // score: acceptable label-index band
}

// UnmarshalYAML keeps YAML parsing, validation and request serialization in
// agreement on one schema while decoding the polymorphic `criteria` field by
// question type.
func (q *Question) UnmarshalYAML(value *yaml.Node) error {
	var aux struct {
		Type         string     `yaml:"type"`
		Instructions string     `yaml:"instructions"`
		Criteria     yaml.Node  `yaml:"criteria"`
		Pass         *ScorePass `yaml:"pass"`
	}
	if err := value.Decode(&aux); err != nil {
		return err
	}
	q.Type = aux.Type
	if q.Type == "" {
		q.Type = "noul"
	}
	q.Instructions = aux.Instructions
	q.Pass = aux.Pass

	switch q.Type {
	case "noul":
		if aux.Criteria.Kind != 0 {
			if err := aux.Criteria.Decode(&q.NoulCriteria); err != nil {
				return fmt.Errorf("noul question criteria must be a map with true/false descriptions: %w", err)
			}
		}
	case "score":
		if aux.Criteria.Kind != 0 {
			if err := aux.Criteria.Decode(&q.ScoreLabels); err != nil {
				return fmt.Errorf("score question criteria must be an ordered list of labels: %w", err)
			}
		}
	}
	return nil
}

type Evaluation struct {
	Mode        string              `yaml:"mode"`        // hard | soft
	Threshold   *float64            `yaml:"threshold"`   // percentage 0..100 in YAML
	Aggregation string              `yaml:"aggregation"` // soft only: minimum (default) | weighted_average
	Criteria    []string            `yaml:"criteria"`
	Questions   map[string]Question `yaml:"questions"` // explicit Jev questions
}

type Test struct {
	ID           string         `yaml:"id"`
	SystemPrompt string         `yaml:"system_prompt"`
	UserPrompt   string         `yaml:"user_prompt"`
	Tags         []string       `yaml:"tags"`
	Metadata     map[string]any `yaml:"metadata"`
	Evaluation   Evaluation     `yaml:"evaluation"`
}

// raw mirrors the on-disk shape before defaults are merged.
type raw struct {
	Suite struct {
		Name string `yaml:"name"`
	} `yaml:"suite"`
	Runner    RunnerConfig    `yaml:"runner"`
	Evaluator EvaluatorConfig `yaml:"evaluator"`
	Defaults  struct {
		Evaluation Evaluation `yaml:"evaluation"`
	} `yaml:"defaults"`
	Tests []Test `yaml:"tests"`
}

// Load reads, parses and validates a suite file, merging suite-level defaults
// into each test. CLI overrides are applied separately by the caller.
func Load(path string) (*Suite, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r raw
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	s := &Suite{
		Name:      r.Suite.Name,
		Runner:    r.Runner,
		Evaluator: r.Evaluator,
		Tests:     r.Tests,
	}
	if s.Evaluator.Type == "" {
		s.Evaluator.Type = "mock"
	}
	for i := range s.Tests {
		mergeDefaults(&s.Tests[i].Evaluation, r.Defaults.Evaluation)
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return s, nil
}

func mergeDefaults(e *Evaluation, def Evaluation) {
	if e.Mode == "" {
		e.Mode = def.Mode
	}
	if e.Mode == "" {
		e.Mode = "soft"
	}
	if e.Threshold == nil {
		e.Threshold = def.Threshold
	}
	if len(e.Criteria) == 0 {
		e.Criteria = def.Criteria
	}
	if e.Aggregation == "" {
		e.Aggregation = def.Aggregation
	}
	if e.Aggregation == "" {
		e.Aggregation = "minimum"
	}
}

// Validate reports useful, aggregated validation errors.
func (s *Suite) Validate() error {
	var errs []string
	if s.Name == "" {
		errs = append(errs, "suite.name is required")
	}
	switch s.Runner.Type {
	case "http":
		if s.Runner.Endpoint == "" {
			errs = append(errs, "runner.endpoint is required for http runner")
		}
	case "command":
		if s.Runner.Command == "" {
			errs = append(errs, "runner.command is required for command runner")
		}
	case "":
		errs = append(errs, "runner.type is required (http|command)")
	default:
		errs = append(errs, fmt.Sprintf("runner.type %q is not supported (http|command)", s.Runner.Type))
	}

	switch s.Evaluator.Type {
	case "openrouter", "jev":
		// endpoint/model fall back to env then default; OPENROUTER_API_KEY is
		// checked at call time.
	case "command":
		if s.Evaluator.Command == "" {
			errs = append(errs, "evaluator.command is required for command evaluator")
		}
	case "mock":
	default:
		errs = append(errs, fmt.Sprintf("evaluator.type %q is not supported (openrouter|command|mock)", s.Evaluator.Type))
	}

	if len(s.Tests) == 0 {
		errs = append(errs, "at least one test is required")
	}
	seen := map[string]bool{}
	for i, t := range s.Tests {
		where := fmt.Sprintf("tests[%d]", i)
		if t.ID == "" {
			errs = append(errs, where+".id is required")
		} else {
			if seen[t.ID] {
				errs = append(errs, fmt.Sprintf("duplicate test id %q", t.ID))
			}
			seen[t.ID] = true
			where = "test " + t.ID
		}
		if t.UserPrompt == "" {
			errs = append(errs, where+": user_prompt is required")
		}
		switch t.Evaluation.Mode {
		case "hard", "soft":
		default:
			errs = append(errs, fmt.Sprintf("%s: evaluation.mode %q is not supported (hard|soft)", where, t.Evaluation.Mode))
		}
		if len(t.Evaluation.Criteria) == 0 && len(t.Evaluation.Questions) == 0 {
			errs = append(errs, where+": evaluation needs at least one criterion or explicit question")
		}
		switch t.Evaluation.Aggregation {
		case "", "minimum", "weighted_average":
		default:
			errs = append(errs, fmt.Sprintf("%s: aggregation %q is not supported (minimum|weighted_average)", where, t.Evaluation.Aggregation))
		}
		for name, q := range t.Evaluation.Questions {
			if q.Instructions == "" {
				errs = append(errs, fmt.Sprintf("%s: question %q needs instructions", where, name))
			}
			switch q.Type {
			case "noul":
				if q.NoulCriteria != nil {
					if _, ok := q.NoulCriteria["true"]; !ok {
						errs = append(errs, fmt.Sprintf("%s: noul question %q criteria must include a \"true\" description", where, name))
					}
					if _, ok := q.NoulCriteria["false"]; !ok {
						errs = append(errs, fmt.Sprintf("%s: noul question %q criteria must include a \"false\" description", where, name))
					}
				}
			case "score":
				errs = append(errs, validateScoreQuestion(where, name, q)...)
			case "choice":
				errs = append(errs, fmt.Sprintf("%s: question %q type \"choice\" is not supported", where, name))
			default:
				errs = append(errs, fmt.Sprintf("%s: question %q type %q not supported (noul|score)", where, name, q.Type))
			}
		}
		if th := t.Evaluation.Threshold; th != nil && (*th < 0 || *th > 100) {
			errs = append(errs, fmt.Sprintf("%s: threshold %.1f out of range 0..100", where, *th))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid suite:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// validateScoreQuestion checks a score question's labels and acceptable band.
func validateScoreQuestion(where, name string, q Question) []string {
	var errs []string
	n := len(q.ScoreLabels)
	if n < 2 {
		errs = append(errs, fmt.Sprintf("%s: score question %q needs at least two ordered labels in criteria", where, name))
	}
	for i, l := range q.ScoreLabels {
		if strings.TrimSpace(l) == "" {
			errs = append(errs, fmt.Sprintf("%s: score question %q label %d is empty", where, name, i))
		}
	}
	if q.Pass == nil || (q.Pass.Min == nil && q.Pass.Max == nil) {
		errs = append(errs, fmt.Sprintf("%s: score question %q needs an explicit pass band (pass.min and/or pass.max)", where, name))
		return errs
	}
	maxIdx := float64(n - 1)
	if q.Pass.Min != nil && (*q.Pass.Min < 0 || *q.Pass.Min > maxIdx) {
		errs = append(errs, fmt.Sprintf("%s: score question %q pass.min %.2f out of range 0..%d", where, name, *q.Pass.Min, n-1))
	}
	if q.Pass.Max != nil && (*q.Pass.Max < 0 || *q.Pass.Max > maxIdx) {
		errs = append(errs, fmt.Sprintf("%s: score question %q pass.max %.2f out of range 0..%d", where, name, *q.Pass.Max, n-1))
	}
	if q.Pass.Min != nil && q.Pass.Max != nil && *q.Pass.Min > *q.Pass.Max {
		errs = append(errs, fmt.Sprintf("%s: score question %q pass.min %.2f exceeds pass.max %.2f", where, name, *q.Pass.Min, *q.Pass.Max))
	}
	return errs
}

// Threshold returns the per-test threshold as a 0..1 fraction, defaulting to
// 0.5 when unset.
func (t Test) Threshold() float64 {
	if t.Evaluation.Threshold == nil {
		return 0.5
	}
	return *t.Evaluation.Threshold / 100
}
