// Package scheduler runs tests concurrently through a bounded worker pool and
// turns responses + scores into normalized results. Its Decide/Evaluate
// helpers are the single evaluation engine shared with runtime mode.
package scheduler

import (
	"context"
	"sort"
	"time"

	"github.com/mhenni13/ci-eval-llm/internal/config"
	"github.com/mhenni13/ci-eval-llm/internal/evaluator"
	"github.com/mhenni13/ci-eval-llm/internal/results"
	"github.com/mhenni13/ci-eval-llm/internal/runner"
)

// Options tune a run. Mode/Threshold here are CLI overrides; empty/<0 means
// "use each test's own setting".
type Options struct {
	Workers     int
	Mode        string
	Threshold   float64 // 0..1, negative = unset
	Timeout     time.Duration
	Retries     int
	BaseBackoff time.Duration
}

func (o Options) withDefaults() Options {
	if o.Workers <= 0 {
		o.Workers = 10
	}
	if o.Timeout <= 0 {
		o.Timeout = 30 * time.Second
	}
	if o.Retries < 0 {
		o.Retries = 0
	}
	if o.BaseBackoff <= 0 {
		o.BaseBackoff = 100 * time.Millisecond
	}
	return o
}

// Decide maps a score to PASS/FAIL. It is the authoritative pass rule and is
// reused verbatim by runtime mode.
func Decide(mode evaluator.Mode, score, threshold float64) results.Status {
	if mode == evaluator.Hard {
		if score >= 1 {
			return results.Pass
		}
		return results.Fail
	}
	if score >= threshold {
		return results.Pass
	}
	return results.Fail
}

// Run executes specs concurrently and returns one result per spec. A failure
// in one test never affects another; cancellation leaves unprocessed tests as
// SKIPPED.
func Run(ctx context.Context, suite *config.Suite, r runner.Runner, e evaluator.Evaluator, opts Options, runID string) []results.Result {
	opts = opts.withDefaults()

	type job struct {
		idx  int
		spec config.Test
	}
	out := make([]results.Result, len(suite.Tests))
	// Pre-fill as SKIPPED so cancelled/unreached tests are explicit.
	for i, t := range suite.Tests {
		out[i] = results.Result{
			TestID: t.ID, Suite: suite.Name, Source: "ci", RunID: runID,
			Status: results.Skipped, Mode: effectiveMode(t, opts), Timestamp: time.Now(),
		}
	}

	jobs := make(chan job)
	done := make(chan struct {
		idx int
		res results.Result
	})

	workers := opts.Workers
	if workers > len(suite.Tests) && len(suite.Tests) > 0 {
		workers = len(suite.Tests)
	}
	for w := 0; w < workers; w++ {
		go func() {
			for j := range jobs {
				done <- struct {
					idx int
					res results.Result
				}{j.idx, runOne(ctx, suite, r, e, opts, runID, j.spec)}
			}
		}()
	}

	// Feed jobs, honoring cancellation.
	go func() {
		defer close(jobs)
		for i, t := range suite.Tests {
			select {
			case <-ctx.Done():
				return
			case jobs <- job{i, t}:
			}
		}
	}()

	for completed := 0; completed < len(suite.Tests); {
		select {
		case d := <-done:
			out[d.idx] = d.res
			completed++
		case <-ctx.Done():
			// Drain nothing more; remaining stay SKIPPED. Still collect any
			// in-flight completions already queued.
			for completed < len(suite.Tests) {
				select {
				case d := <-done:
					out[d.idx] = d.res
					completed++
				default:
					return out
				}
			}
		}
	}
	return out
}

func effectiveMode(t config.Test, opts Options) string {
	if opts.Mode != "" {
		return opts.Mode
	}
	return t.Evaluation.Mode
}

func effectiveThreshold(t config.Test, opts Options) float64 {
	if opts.Threshold >= 0 {
		return opts.Threshold
	}
	return t.Threshold()
}

func runOne(ctx context.Context, suite *config.Suite, r runner.Runner, e evaluator.Evaluator, opts Options, runID string, t config.Test) results.Result {
	start := time.Now()
	mode := evaluator.Mode(effectiveMode(t, opts))
	threshold := effectiveThreshold(t, opts)
	res := results.Result{
		TestID: t.ID, Suite: suite.Name, Source: "ci", RunID: runID,
		Mode: string(mode), Aggregation: t.Evaluation.Aggregation,
		SystemPrompt: t.SystemPrompt, UserPrompt: t.UserPrompt,
		Threshold: threshold, Timestamp: start, Metadata: t.Metadata,
	}

	tctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	var resp runner.Response
	err := retry(tctx, opts, func() error {
		var e2 error
		resp, e2 = r.Run(tctx, runner.Request{
			TestID: t.ID, SystemPrompt: t.SystemPrompt, UserPrompt: t.UserPrompt, Metadata: t.Metadata,
		})
		return e2
	})
	if err != nil {
		res.Status = results.Error
		res.Error = "runner: " + err.Error()
		res.LatencyMS = time.Since(start).Milliseconds()
		return res
	}
	res.Response = resp.Response

	var ev evaluator.Result
	err = retry(tctx, opts, func() error {
		var e2 error
		ev, e2 = e.Evaluate(tctx, evaluator.Input{
			TestID: t.ID, SystemPrompt: t.SystemPrompt, UserPrompt: t.UserPrompt,
			Response: resp.Response, Criteria: t.Evaluation.Criteria,
			Questions: toQuestions(t.Evaluation.Questions), Mode: mode,
			Aggregation: t.Evaluation.Aggregation,
		})
		return e2
	})
	res.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		res.Status = results.Error
		res.Error = "evaluator: " + err.Error()
		return res
	}
	res.Score = ev.Score
	res.Reason = ev.Reason
	res.Criteria = ev.Criteria
	res.Status = Decide(mode, ev.Score, threshold)
	return res
}

// toQuestions converts the YAML question map into an ordered, stable slice.
func toQuestions(m map[string]config.Question) []evaluator.Question {
	if len(m) == 0 {
		return nil
	}
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]evaluator.Question, 0, len(m))
	for _, name := range names {
		q := m[name]
		eq := evaluator.Question{
			Name: name, Type: q.Type, Instructions: q.Instructions,
			NoulCriteria: q.NoulCriteria, ScoreLabels: q.ScoreLabels,
		}
		if q.Pass != nil {
			eq.PassMin, eq.PassMax = q.Pass.Min, q.Pass.Max
		}
		out = append(out, eq)
	}
	return out
}

// retry runs fn with bounded attempts and exponential backoff, aborting on
// context cancellation.
func retry(ctx context.Context, opts Options, fn func() error) error {
	var err error
	for attempt := 0; attempt <= opts.Retries; attempt++ {
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = fn(); err == nil {
			return nil
		}
		if attempt == opts.Retries {
			break
		}
		backoff := opts.BaseBackoff << attempt
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
	return err
}
