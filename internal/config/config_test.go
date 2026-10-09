package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "suite.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const valid = `
suite:
  name: s
runner:
  type: command
  command: ./app
defaults:
  evaluation:
    mode: soft
    threshold: 80
tests:
  - id: t1
    user_prompt: hi
    evaluation:
      criteria: [must be nice]
  - id: t2
    user_prompt: bye
    evaluation:
      mode: hard
      criteria: [must be safe]
`

func TestLoadMergesDefaults(t *testing.T) {
	s, err := Load(write(t, valid))
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "s" || len(s.Tests) != 2 {
		t.Fatalf("bad parse: %+v", s)
	}
	// t1 inherits soft/80 from defaults.
	if s.Tests[0].Evaluation.Mode != "soft" {
		t.Errorf("t1 mode = %q", s.Tests[0].Evaluation.Mode)
	}
	if got := s.Tests[0].Threshold(); got != 0.80 {
		t.Errorf("t1 threshold = %v want 0.80", got)
	}
	// t2 overrides mode to hard but still inherits threshold default.
	if s.Tests[1].Evaluation.Mode != "hard" {
		t.Errorf("t2 mode = %q", s.Tests[1].Evaluation.Mode)
	}
	// evaluator defaults to mock when omitted.
	if s.Evaluator.Type != "mock" {
		t.Errorf("evaluator type = %q", s.Evaluator.Type)
	}
}

func TestValidationErrors(t *testing.T) {
	cases := map[string]string{
		"missing name": `
runner: {type: command, command: x}
tests: [{id: a, user_prompt: u, evaluation: {mode: hard}}]`,
		"unknown runner": `
suite: {name: s}
runner: {type: carrier}
tests: [{id: a, user_prompt: u, evaluation: {mode: hard}}]`,
		"duplicate id": `
suite: {name: s}
runner: {type: command, command: x}
tests:
  - {id: dup, user_prompt: u, evaluation: {mode: hard}}
  - {id: dup, user_prompt: u, evaluation: {mode: hard}}`,
		"soft without criteria": `
suite: {name: s}
runner: {type: command, command: x}
tests: [{id: a, user_prompt: u, evaluation: {mode: soft, threshold: 50}}]`,
		"missing user_prompt": `
suite: {name: s}
runner: {type: command, command: x}
tests: [{id: a, evaluation: {mode: hard}}]`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(write(t, body)); err == nil {
				t.Fatal("expected validation error, got nil")
			}
		})
	}
}

func TestNoulAndScoreQuestionParsing(t *testing.T) {
	s, err := Load(write(t, `
suite: {name: s}
runner: {type: command, command: x}
evaluator: {type: openrouter}
tests:
  - id: t
    user_prompt: u
    evaluation:
      mode: soft
      threshold: 80
      questions:
        correct:
          type: noul
          instructions: Is it correct?
          criteria: {"true": "answer is correct", "false": "answer is wrong"}
        frustration:
          type: score
          instructions: How frustrated is the customer?
          criteria: ["Calm", "Frustrated", "Very angry"]
          pass: {max: 1}
`))
	if err != nil {
		t.Fatal(err)
	}
	qs := s.Tests[0].Evaluation.Questions
	if qs["correct"].Type != "noul" || qs["correct"].NoulCriteria["true"] != "answer is correct" {
		t.Errorf("noul question parsed wrong: %+v", qs["correct"])
	}
	fr := qs["frustration"]
	if fr.Type != "score" || len(fr.ScoreLabels) != 3 || fr.ScoreLabels[2] != "Very angry" {
		t.Errorf("score labels parsed wrong: %+v", fr)
	}
	if fr.Pass == nil || fr.Pass.Max == nil || *fr.Pass.Max != 1 {
		t.Errorf("score pass band parsed wrong: %+v", fr.Pass)
	}
}

func TestQuestionValidationErrors(t *testing.T) {
	cases := map[string]string{
		"choice rejected": `
suite: {name: s}
runner: {type: command, command: x}
tests:
  - id: t
    user_prompt: u
    evaluation:
      mode: hard
      questions:
        dept: {type: choice, instructions: which team}`,
		"score without pass band": `
suite: {name: s}
runner: {type: command, command: x}
tests:
  - id: t
    user_prompt: u
    evaluation:
      mode: hard
      questions:
        q: {type: score, instructions: rate, criteria: ["a", "b"]}`,
		"score single label": `
suite: {name: s}
runner: {type: command, command: x}
tests:
  - id: t
    user_prompt: u
    evaluation:
      mode: hard
      questions:
        q: {type: score, instructions: rate, criteria: ["only"], pass: {max: 0}}`,
		"score pass out of range": `
suite: {name: s}
runner: {type: command, command: x}
tests:
  - id: t
    user_prompt: u
    evaluation:
      mode: hard
      questions:
        q: {type: score, instructions: rate, criteria: ["a", "b"], pass: {max: 5}}`,
		"noul missing true key": `
suite: {name: s}
runner: {type: command, command: x}
tests:
  - id: t
    user_prompt: u
    evaluation:
      mode: hard
      questions:
        q: {type: noul, instructions: ok, criteria: {"false": "no"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(write(t, body)); err == nil {
				t.Fatalf("expected validation error for %q", name)
			}
		})
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	_, err := Load(write(t, `
suite: {name: s}
runner: {type: command, command: x}
tests: [{id: a, user_prompt: u, typpo: 1, evaluation: {mode: hard}}]`))
	if err == nil || !strings.Contains(err.Error(), "typpo") {
		t.Fatalf("expected unknown-field error, got %v", err)
	}
}
