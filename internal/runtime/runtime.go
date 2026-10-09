// Package runtime ingests completed LLM interactions from a production
// application and evaluates them asynchronously, reusing the CI evaluation
// engine. The app never shares its LLM credentials with this tool.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/mhenni13/ci-eval-llm/internal/evaluator"
	"github.com/mhenni13/ci-eval-llm/internal/results"
	"github.com/mhenni13/ci-eval-llm/internal/scheduler"
)

// Event is a completed interaction pushed by the production application.
type Event struct {
	ID           string            `json:"id"`
	SystemPrompt string            `json:"system_prompt"`
	UserPrompt   string            `json:"user_prompt"`
	Response     string            `json:"response"`
	Criteria     []string          `json:"criteria"`
	Questions    map[string]qInput `json:"questions"`
	Mode         string            `json:"mode"`
	Aggregation  string            `json:"aggregation"`
	Threshold    *float64          `json:"threshold"` // 0..1
	Metadata     map[string]any    `json:"metadata"`
	Metrics      struct {
		LatencyMS int64 `json:"latency_ms"`
	} `json:"metrics"`
	Satisfaction *int   `json:"satisfaction"`
	Feedback     string `json:"feedback"`
}

// qInput mirrors an explicit Jev question in a runtime event (noul or score).
type qInput struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	NoulCriteria map[string]string `json:"noul_criteria,omitempty"`
	ScoreLabels  []string          `json:"score_labels,omitempty"`
	PassMin      *float64          `json:"pass_min,omitempty"`
	PassMax      *float64          `json:"pass_max,omitempty"`
}

func (ev Event) questions() []evaluator.Question {
	if len(ev.Questions) == 0 {
		return nil
	}
	names := make([]string, 0, len(ev.Questions))
	for n := range ev.Questions {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]evaluator.Question, 0, len(names))
	for _, n := range names {
		q := ev.Questions[n]
		out = append(out, evaluator.Question{
			Name: n, Type: q.Type, Instructions: q.Instructions,
			NoulCriteria: q.NoulCriteria, ScoreLabels: q.ScoreLabels,
			PassMin: q.PassMin, PassMax: q.PassMax,
		})
	}
	return out
}

// Server accepts events and evaluates them on a bounded worker pool.
type Server struct {
	eval    evaluator.Evaluator
	store   *results.Store
	queue   chan Event
	wg      sync.WaitGroup
	workers int
	timeout time.Duration
}

func NewServer(eval evaluator.Evaluator, store *results.Store, workers, queueSize int) *Server {
	if workers <= 0 {
		workers = 4
	}
	if queueSize <= 0 {
		queueSize = 1024
	}
	return &Server{
		eval:    eval,
		store:   store,
		queue:   make(chan Event, queueSize),
		workers: workers,
		timeout: 30 * time.Second,
	}
}

func (s *Server) startWorkers() {
	for i := 0; i < s.workers; i++ {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			for ev := range s.queue {
				s.evaluate(ev)
			}
		}()
	}
}

func (s *Server) evaluate(ev Event) {
	mode := evaluator.Mode(ev.Mode)
	if mode == "" {
		mode = evaluator.Soft
	}
	threshold := 0.5
	if ev.Threshold != nil {
		threshold = *ev.Threshold
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()

	r := results.Result{
		TestID: ev.ID, Suite: "runtime", Source: "runtime",
		Mode: string(mode), Aggregation: ev.Aggregation, Response: ev.Response, Threshold: threshold,
		LatencyMS: ev.Metrics.LatencyMS, Timestamp: time.Now(),
		Metadata: ev.Metadata, Satisfaction: ev.Satisfaction, Feedback: ev.Feedback,
	}
	res, err := s.eval.Evaluate(ctx, evaluator.Input{
		TestID: ev.ID, SystemPrompt: ev.SystemPrompt, UserPrompt: ev.UserPrompt,
		Response: ev.Response, Criteria: ev.Criteria, Questions: ev.questions(),
		Mode: mode, Aggregation: ev.Aggregation,
	})
	if err != nil {
		r.Status = results.Error
		r.Error = err.Error()
	} else {
		r.Score = res.Score
		r.Reason = res.Reason
		r.Criteria = res.Criteria
		r.Status = scheduler.Decide(mode, res.Score, threshold)
	}
	if err := s.store.AppendRuntime(r); err != nil {
		fmt.Fprintf(os.Stderr, "runtime: persist failed: %v\n", err)
	}
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/evaluations", func(w http.ResponseWriter, req *http.Request) {
		var ev Event
		if err := json.NewDecoder(req.Body).Decode(&ev); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		if ev.ID == "" || ev.Response == "" {
			http.Error(w, "id and response are required", http.StatusBadRequest)
			return
		}
		// Async: enqueue and return immediately so we add no production latency.
		select {
		case s.queue <- ev:
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]string{"status": "queued", "id": ev.ID})
		default:
			http.Error(w, "evaluation queue full", http.StatusServiceUnavailable)
		}
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"status": "ok", "queued": len(s.queue)})
	})
	return mux
}

// ListenAndServe runs the server until ctx is cancelled, then drains the queue.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	s.startWorkers()
	srv := &http.Server{Addr: addr, Handler: s.handler()}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutCtx)
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			close(s.queue)
			s.wg.Wait()
			return err
		}
	}
	// Graceful drain: stop accepting, finish queued evaluations.
	close(s.queue)
	s.wg.Wait()
	return nil
}

// --- process control: start/status/stop via a pidfile under the store dir ---

type procInfo struct {
	PID  int    `json:"pid"`
	Addr string `json:"addr"`
}

func pidPath(dir string) string { return filepath.Join(dir, "runtime", "server.json") }

// WritePID records the running server for status/stop.
func WritePID(dir, addr string) error {
	p := pidPath(dir)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(procInfo{PID: os.Getpid(), Addr: addr})
	return os.WriteFile(p, b, 0o644)
}

func RemovePID(dir string) { os.Remove(pidPath(dir)) }

func readPID(dir string) (procInfo, error) {
	b, err := os.ReadFile(pidPath(dir))
	if err != nil {
		return procInfo{}, err
	}
	var p procInfo
	return p, json.Unmarshal(b, &p)
}

// Status reports whether a runtime server is running.
func Status(dir string) (procInfo, bool) {
	p, err := readPID(dir)
	if err != nil {
		return procInfo{}, false
	}
	return p, processAlive(p.PID)
}

// Stop terminates a running runtime server.
func Stop(dir string) error {
	p, running := Status(dir)
	if !running {
		return errors.New("no running runtime server")
	}
	proc, err := os.FindProcess(p.PID)
	if err != nil {
		return err
	}
	if err := proc.Signal(syscallTerm()); err != nil {
		return err
	}
	RemovePID(dir)
	return nil
}

// AddrString is a tiny helper so callers don't import strconv.
func AddrString(host string, port int) string { return net.JoinHostPort(host, strconv.Itoa(port)) }
