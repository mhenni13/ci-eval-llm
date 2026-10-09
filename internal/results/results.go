// Package results defines the normalized result model and local JSONL storage
// shared by CI and runtime modes.
package results

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type Status string

const (
	Pass    Status = "PASS"
	Fail    Status = "FAIL"
	Error   Status = "ERROR"
	Skipped Status = "SKIPPED"
)

// CriterionResult is one atomic evaluation signal (one Jev question). The
// per-criterion breakdown is preserved so the CI report can show exactly which
// criteria failed instead of hiding them behind a single aggregate score.
//
// Two Jev question types are represented. Noul questions carry a probability in
// Noul; score questions carry the raw floating-point ScoreValue plus the ordered
// Legend and per-label Probabilities. Both populate the normalized Score
// (P(criterion acceptable) in [0,1]) and Pass used by aggregation and gating.
type CriterionResult struct {
	Name          string             `json:"name"`
	Type          string             `json:"type,omitempty"` // "noul" | "score" ("" == legacy noul)
	Instructions  string             `json:"instructions,omitempty"`
	Noul          *float64           `json:"noul,omitempty"`          // noul: probability in [0,1]
	ScoreValue    *float64           `json:"score_value,omitempty"`   // score: raw float on the ordered scale (unrounded)
	Legend        map[string]string  `json:"legend,omitempty"`        // score: index -> label
	Probabilities map[string]float64 `json:"probabilities,omitempty"` // score: index -> probability
	Confidence    *float64           `json:"confidence,omitempty"`    // optional, both types (NOT correctness)
	Score         float64            `json:"score"`                   // normalized P(acceptable) in [0,1], for aggregation
	Pass          bool               `json:"pass"`
}

// Detail renders a short, type-aware description of the criterion's raw signal
// for CLI/report output.
func (c CriterionResult) Detail() string {
	switch c.Type {
	case "score":
		s := "score=?"
		if c.ScoreValue != nil {
			s = fmt.Sprintf("score=%.2f", *c.ScoreValue)
		}
		if lbl := c.topLabel(); lbl != "" {
			s += " (" + lbl + ")"
		}
		return s
	default: // noul / legacy
		if c.Noul != nil {
			return fmt.Sprintf("noul=%.3f", *c.Noul)
		}
		return "noul=?"
	}
}

// topLabel returns the legend label of the most probable score bucket, if known.
func (c CriterionResult) topLabel() string {
	if len(c.Probabilities) == 0 || len(c.Legend) == 0 {
		return ""
	}
	best, bestP := "", -1.0
	for idx, p := range c.Probabilities {
		if p > bestP {
			best, bestP = idx, p
		}
	}
	return c.Legend[best]
}

// Result is the normalized record persisted for every evaluation, from either
// CI runs or runtime ingestion.
type Result struct {
	TestID       string            `json:"test_id"`
	Suite        string            `json:"suite"`
	Source       string            `json:"source"` // "ci" | "runtime"
	RunID        string            `json:"run_id"`
	Status       Status            `json:"status"`
	Mode         string            `json:"mode"`
	Aggregation  string            `json:"aggregation,omitempty"`
	SystemPrompt string            `json:"system_prompt,omitempty"`
	UserPrompt   string            `json:"user_prompt,omitempty"`
	Response     string            `json:"response,omitempty"`
	Score        float64           `json:"score"`
	Threshold    float64           `json:"threshold"`
	Reason       string            `json:"reason,omitempty"`
	Criteria     []CriterionResult `json:"criteria,omitempty"`
	LatencyMS    int64             `json:"latency_ms"`
	Error        string            `json:"error,omitempty"`
	Timestamp    time.Time         `json:"timestamp"`
	Metadata     map[string]any    `json:"metadata,omitempty"`

	// Runtime-only, kept separate from the quality score on purpose.
	Satisfaction *int   `json:"satisfaction,omitempty"`
	Feedback     string `json:"feedback,omitempty"`
}

// Store appends results as JSONL under a directory, one file per run plus a
// shared runtime file. Chosen over SQLite to avoid a cgo dependency; the shape
// is dashboard-ready.
type Store struct {
	dir string
	mu  sync.Mutex
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// WriteRun persists a batch of CI results under runs/<runID>.jsonl.
func (s *Store) WriteRun(runID string, rs []Result) error {
	path := filepath.Join(s.dir, "runs", runID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return appendJSONL(path, rs)
}

// AppendRuntime persists a single runtime evaluation.
func (s *Store) AppendRuntime(r Result) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.dir, "runtime", "events.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return appendJSONL(path, []Result{r})
}

// LoadAll reads every stored result (runs + runtime), newest files last.
func (s *Store) LoadAll() ([]Result, error) {
	var out []Result
	for _, sub := range []string{"runs", "runtime"} {
		files, _ := filepath.Glob(filepath.Join(s.dir, sub, "*.jsonl"))
		sort.Strings(files)
		for _, f := range files {
			rs, err := readJSONL(f)
			if err != nil {
				return nil, err
			}
			out = append(out, rs...)
		}
	}
	return out, nil
}

func appendJSONL(path string, rs []Result) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, r := range rs {
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "%s\n", b); err != nil {
			return err
		}
	}
	return w.Flush()
}

func readJSONL(path string) ([]Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Result
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r Result
		if err := json.Unmarshal(line, &r); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		out = append(out, r)
	}
	return out, sc.Err()
}
