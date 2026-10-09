// Command ci-eval-llm runs YAML LLM test suites and ingests runtime evaluations.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mhenni13/ci-eval-llm/internal/config"
	"github.com/mhenni13/ci-eval-llm/internal/evaluator"
	"github.com/mhenni13/ci-eval-llm/internal/report"
	"github.com/mhenni13/ci-eval-llm/internal/results"
	"github.com/mhenni13/ci-eval-llm/internal/runner"
	"github.com/mhenni13/ci-eval-llm/internal/runtime"
	"github.com/mhenni13/ci-eval-llm/internal/scheduler"
)

const usage = `ci-eval-llm — LLM behavioral testing and runtime evaluation

Usage:
  ci-eval-llm run <suite.yaml> [--mode hard|soft] [--threshold 80] [--workers N] [--json]
  ci-eval-llm report [--json] [--store DIR]
  ci-eval-llm runtime start [--addr HOST:PORT] [--workers N]
  ci-eval-llm runtime status
  ci-eval-llm runtime stop

Exit code is 0 only when every test passed.`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		os.Exit(cmdRun(os.Args[2:]))
	case "report":
		os.Exit(cmdReport(os.Args[2:]))
	case "runtime":
		os.Exit(cmdRuntime(os.Args[2:]))
	case "-h", "--help", "help":
		fmt.Println(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s\n", os.Args[1], usage)
		os.Exit(2)
	}
}

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	mode := fs.String("mode", "", "override evaluation mode (hard|soft)")
	threshold := fs.Float64("threshold", -1, "override pass threshold as percentage (e.g. 80 = 0.80)")
	workers := fs.Int("workers", 10, "number of concurrent workers")
	timeout := fs.Duration("timeout", 30*time.Second, "per-test timeout")
	retries := fs.Int("retries", 2, "bounded retries on runner/evaluator error")
	store := fs.String("store", ".ci-eval-llm", "results storage directory")
	asJSON := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Parse(args)

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "run: missing suite file\n\n"+usage)
		return 2
	}
	suite, err := config.Load(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	r, err := runner.New(suite.Runner)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ev, err := evaluator.New(suite.Evaluator)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	st, err := results.Open(*store)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	opts := scheduler.Options{
		Workers: *workers, Mode: *mode, Threshold: -1,
		Timeout: *timeout, Retries: *retries,
	}
	if *threshold >= 0 {
		opts.Threshold = *threshold / 100 // CLI percentage -> fraction
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runID := time.Now().UTC().Format("20060102T150405") + "-" + suite.Name
	start := time.Now()
	rs := scheduler.Run(ctx, suite, r, ev, opts, runID)
	dur := time.Since(start)

	if err := st.WriteRun(runID, rs); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to persist results: %v\n", err)
	}

	if *asJSON {
		emitJSON(rs, dur)
	} else {
		printRunSummary(suite.Name, rs, dur)
	}
	return exitCode(rs)
}

func printRunSummary(suite string, rs []results.Result, dur time.Duration) {
	var pass, fail, errc, skip int
	for _, r := range rs {
		switch r.Status {
		case results.Pass:
			pass++
		case results.Fail:
			fail++
		case results.Error:
			errc++
		case results.Skipped:
			skip++
		}
	}
	evaluated := pass + fail
	rate := 0.0
	if evaluated > 0 {
		rate = float64(pass) / float64(evaluated) * 100
	}
	tp := 0.0
	if dur.Seconds() > 0 {
		tp = float64(len(rs)) / dur.Seconds()
	}

	fmt.Println("\nLLM Evaluation")
	fmt.Println("──────────────")
	fmt.Printf("Suite: %s\n\n", suite)
	fmt.Printf("Tests:     %6d\n", len(rs))
	fmt.Printf("Passed:    %6d\n", pass)
	fmt.Printf("Failed:    %6d\n", fail)
	fmt.Printf("Errors:    %6d\n", errc)
	if skip > 0 {
		fmt.Printf("Skipped:   %6d\n", skip)
	}
	fmt.Printf("Pass rate: %6.1f%%\n", rate)
	fmt.Printf("Duration:  %6s\n", dur.Round(time.Millisecond))
	fmt.Printf("Throughput:%6.1f tests/s\n", tp)

	for _, r := range rs {
		if r.Status == results.Fail {
			fmt.Printf("\nFAILED %s\n\n", r.TestID)
			fmt.Printf("Score:     %.2f\n", r.Score)
			fmt.Printf("Threshold: %.2f\n", r.Threshold)
			if failing := failingCriteria(r); len(failing) > 0 {
				fmt.Printf("\nFailed criteria:\n")
				for _, c := range failing {
					fmt.Printf("  ✗ %s (%s)\n", criterionLabel(c), c.Detail())
				}
			} else if r.Reason != "" {
				fmt.Printf("\nReason:\n%s\n", r.Reason)
			}
		} else if r.Status == results.Error {
			fmt.Printf("\nERROR  %s\n\n%s\n", r.TestID, r.Error)
		}
	}
}

func failingCriteria(r results.Result) []results.CriterionResult {
	var out []results.CriterionResult
	for _, c := range r.Criteria {
		if !c.Pass {
			out = append(out, c)
		}
	}
	return out
}

func criterionLabel(c results.CriterionResult) string {
	if c.Instructions != "" {
		return c.Instructions
	}
	return c.Name
}

func emitJSON(rs []results.Result, dur time.Duration) {
	s := report.Build(rs, "ci")
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(map[string]any{
		"summary": map[string]any{
			"total": s.Total, "passed": s.Passed, "failed": s.Failed,
			"errors": s.Errors, "skipped": s.Skipped, "pass_rate": s.PassRate,
			"avg_score": s.AvgScore, "duration_ms": dur.Milliseconds(),
		},
		"results": rs,
	})
}

func exitCode(rs []results.Result) int {
	for _, r := range rs {
		if r.Status != results.Pass {
			return 1
		}
	}
	return 0
}

func cmdReport(args []string) int {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	store := fs.String("store", ".ci-eval-llm", "results storage directory")
	asJSON := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Parse(args)

	st, err := results.Open(*store)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	rs, err := st.LoadAll()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(map[string]any{
			"ci":            report.Build(rs, "ci"),
			"runtime":       report.Build(rs, "runtime"),
			"historical_ci": report.RunCount(rs),
		})
		return 0
	}
	report.WriteText(os.Stdout, rs)
	return 0
}

func cmdRuntime(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "runtime: expected start|status|stop")
		return 2
	}
	sub := args[0]
	fs := flag.NewFlagSet("runtime", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "listen address")
	workers := fs.Int("workers", 4, "evaluation workers")
	store := fs.String("store", ".ci-eval-llm", "results storage directory")
	// Evaluator for runtime: default local mock; override via env.
	evalType := fs.String("evaluator", envOr("CI_EVAL_EVALUATOR", "mock"), "evaluator type (mock|openrouter|command)")
	evalEndpoint := fs.String("evaluator-endpoint", os.Getenv("CI_EVAL_EVALUATOR_ENDPOINT"), "evaluator endpoint (http)")
	evalCommand := fs.String("evaluator-command", os.Getenv("CI_EVAL_EVALUATOR_COMMAND"), "evaluator command")
	fs.Parse(args[1:])

	st, err := results.Open(*store)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	switch sub {
	case "start":
		ev, err := evaluator.New(config.EvaluatorConfig{Type: *evalType, Endpoint: *evalEndpoint, Command: *evalCommand})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		srv := runtime.NewServer(ev, st, *workers, 1024)
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := runtime.WritePID(*store, *addr); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer runtime.RemovePID(*store)
		fmt.Printf("runtime: ingesting on http://%s  (POST /v1/evaluations)\n", *addr)
		if err := srv.ListenAndServe(ctx, *addr); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println("runtime: stopped")
		return 0
	case "status":
		if p, ok := runtime.Status(*store); ok {
			fmt.Printf("running: pid=%d addr=%s\n", p.PID, p.Addr)
			return 0
		}
		fmt.Println("not running")
		return 1
	case "stop":
		if err := runtime.Stop(*store); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println("stopped")
		return 0
	default:
		fmt.Fprintf(os.Stderr, "runtime: unknown subcommand %q\n", sub)
		return 2
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
