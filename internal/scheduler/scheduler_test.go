package scheduler

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mhenni13/ci-eval-llm/internal/config"
	"github.com/mhenni13/ci-eval-llm/internal/evaluator"
	"github.com/mhenni13/ci-eval-llm/internal/results"
	"github.com/mhenni13/ci-eval-llm/internal/runner"
)

type fakeRunner struct {
	fn func(ctx context.Context, req runner.Request) (runner.Response, error)
}

func (f fakeRunner) Run(ctx context.Context, req runner.Request) (runner.Response, error) {
	return f.fn(ctx, req)
}

type fakeEval struct {
	fn func(ctx context.Context, in evaluator.Input) (evaluator.Result, error)
}

func (f fakeEval) Evaluate(ctx context.Context, in evaluator.Input) (evaluator.Result, error) {
	return f.fn(ctx, in)
}

func suiteOf(n int, mode string) *config.Suite {
	s := &config.Suite{Name: "s"}
	for i := 0; i < n; i++ {
		th := 80.0
		s.Tests = append(s.Tests, config.Test{
			ID:         "t" + string(rune('a'+i%26)),
			UserPrompt: "u",
			Evaluation: config.Evaluation{Mode: mode, Threshold: &th, Criteria: []string{"c"}},
		})
	}
	return s
}

func okRunner() runner.Runner {
	return fakeRunner{fn: func(_ context.Context, _ runner.Request) (runner.Response, error) {
		return runner.Response{Response: "ok"}, nil
	}}
}

func scoreEval(score float64) evaluator.Evaluator {
	return fakeEval{fn: func(_ context.Context, _ evaluator.Input) (evaluator.Result, error) {
		return evaluator.Result{Score: score}, nil
	}}
}

func TestDecide(t *testing.T) {
	if Decide(evaluator.Hard, 1, 0) != results.Pass {
		t.Error("hard 1 should pass")
	}
	if Decide(evaluator.Hard, 0, 0) != results.Fail {
		t.Error("hard 0 should fail")
	}
	if Decide(evaluator.Soft, 0.8, 0.8) != results.Pass {
		t.Error("soft >= threshold should pass")
	}
	if Decide(evaluator.Soft, 0.79, 0.8) != results.Fail {
		t.Error("soft < threshold should fail")
	}
}

func TestThresholdHandling(t *testing.T) {
	s := suiteOf(1, "soft")
	// score 0.73 vs test threshold 0.80 -> FAIL
	rs := Run(context.Background(), s, okRunner(), scoreEval(0.73), Options{Workers: 1, Threshold: -1}, "r")
	if rs[0].Status != results.Fail {
		t.Fatalf("want FAIL got %s", rs[0].Status)
	}
	// CLI override threshold to 70 (0.70) -> PASS
	rs = Run(context.Background(), s, okRunner(), scoreEval(0.73), Options{Workers: 1, Threshold: 0.70}, "r")
	if rs[0].Status != results.Pass {
		t.Fatalf("override want PASS got %s", rs[0].Status)
	}
}

func TestRunnerErrorIsolatedAsError(t *testing.T) {
	s := suiteOf(3, "soft")
	calls := int32(0)
	r := fakeRunner{fn: func(_ context.Context, req runner.Request) (runner.Response, error) {
		// Only the second test errors; others must still complete.
		if atomic.AddInt32(&calls, 1); req.TestID == s.Tests[1].ID {
			return runner.Response{}, errors.New("boom")
		}
		return runner.Response{Response: "ok"}, nil
	}}
	rs := Run(context.Background(), s, r, scoreEval(1), Options{Workers: 2, Threshold: -1}, "r")
	var pass, errc int
	for _, x := range rs {
		switch x.Status {
		case results.Pass:
			pass++
		case results.Error:
			errc++
		}
	}
	if pass != 2 || errc != 1 {
		t.Fatalf("pass=%d err=%d", pass, errc)
	}
}

func TestConcurrencyParallelism(t *testing.T) {
	const workers = 8
	s := suiteOf(workers, "soft")
	var active, maxActive int32
	var wg sync.WaitGroup
	wg.Add(workers)
	r := fakeRunner{fn: func(_ context.Context, _ runner.Request) (runner.Response, error) {
		n := atomic.AddInt32(&active, 1)
		for {
			m := atomic.LoadInt32(&maxActive)
			if n <= m || atomic.CompareAndSwapInt32(&maxActive, m, n) {
				break
			}
		}
		wg.Done()
		wg.Wait() // block until all workers are simultaneously in-flight
		atomic.AddInt32(&active, -1)
		return runner.Response{Response: "ok"}, nil
	}}
	done := make(chan struct{})
	go func() {
		Run(context.Background(), s, r, scoreEval(1), Options{Workers: workers, Threshold: -1, Timeout: 5 * time.Second}, "r")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not complete; workers not parallel")
	}
	if maxActive < workers {
		t.Fatalf("max concurrent = %d, want %d (not parallel)", maxActive, workers)
	}
}

func TestTimeout(t *testing.T) {
	s := suiteOf(1, "soft")
	r := fakeRunner{fn: func(ctx context.Context, _ runner.Request) (runner.Response, error) {
		<-ctx.Done()
		return runner.Response{}, ctx.Err()
	}}
	rs := Run(context.Background(), s, r, scoreEval(1), Options{Workers: 1, Threshold: -1, Timeout: 50 * time.Millisecond, Retries: 0}, "r")
	if rs[0].Status != results.Error {
		t.Fatalf("want ERROR on timeout, got %s", rs[0].Status)
	}
}

func TestRetrySucceedsEventually(t *testing.T) {
	s := suiteOf(1, "soft")
	var n int32
	r := fakeRunner{fn: func(_ context.Context, _ runner.Request) (runner.Response, error) {
		if atomic.AddInt32(&n, 1) < 3 {
			return runner.Response{}, errors.New("transient")
		}
		return runner.Response{Response: "ok"}, nil
	}}
	rs := Run(context.Background(), s, r, scoreEval(1), Options{Workers: 1, Threshold: -1, Retries: 3, BaseBackoff: time.Millisecond}, "r")
	if rs[0].Status != results.Pass {
		t.Fatalf("want PASS after retries, got %s (%s)", rs[0].Status, rs[0].Error)
	}
	if n != 3 {
		t.Fatalf("attempts = %d want 3", n)
	}
}

func TestCancellationLeavesSkipped(t *testing.T) {
	s := suiteOf(50, "soft")
	ctx, cancel := context.WithCancel(context.Background())
	var started int32
	r := fakeRunner{fn: func(c context.Context, _ runner.Request) (runner.Response, error) {
		if atomic.AddInt32(&started, 1) == 2 {
			cancel()
		}
		return runner.Response{Response: "ok"}, nil
	}}
	rs := Run(ctx, s, r, scoreEval(1), Options{Workers: 1, Threshold: -1}, "r")
	var skipped int
	for _, x := range rs {
		if x.Status == results.Skipped {
			skipped++
		}
	}
	if skipped == 0 {
		t.Fatal("expected some SKIPPED after cancellation")
	}
}
