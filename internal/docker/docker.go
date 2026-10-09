// Package docker optionally spins up a test environment container before a run
// and tears it down after. It is not required: the MVP runs without Docker.
//
// ponytail: shells out to the `docker` CLI instead of pulling the Docker SDK
// (an enormous dependency tree) to honor "keep dependencies minimal". Swap for
// github.com/docker/docker/client if programmatic control is ever needed.
package docker

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Env describes an optional container to run tests against.
type Env struct {
	Image     string        // docker image
	Name      string        // container name
	Ports     []string      // e.g. "8000:8000"
	Env       []string      // "KEY=VALUE"
	HealthURL string        // optional; polled until 2xx
	Wait      time.Duration // max time to wait for health
}

// Start launches the container (detached). Caller must defer Cleanup.
func (e Env) Start(ctx context.Context) error {
	args := []string{"run", "-d", "--rm", "--name", e.Name}
	for _, p := range e.Ports {
		args = append(args, "-p", p)
	}
	for _, kv := range e.Env {
		args = append(args, "-e", kv)
	}
	args = append(args, e.Image)
	if out, err := run(ctx, args...); err != nil {
		return fmt.Errorf("docker run: %w: %s", err, out)
	}
	if e.HealthURL != "" {
		return e.waitHealthy(ctx)
	}
	return nil
}

func (e Env) waitHealthy(ctx context.Context) error {
	wait := e.Wait
	if wait <= 0 {
		wait = 30 * time.Second
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if healthy(ctx, e.HealthURL) {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("container %s not healthy within %s", e.Name, wait)
}

// Cleanup stops and removes the container (best effort).
func (e Env) Cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	run(ctx, "rm", "-f", e.Name)
}

func run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func healthy(ctx context.Context, url string) bool {
	req, err := newGET(ctx, url)
	if err != nil {
		return false
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
