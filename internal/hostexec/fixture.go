package hostexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
)

// Recording is one captured command result.
type Recording struct {
	Stdout string `json:"stdout"`
	Exit   int    `json:"exit,omitempty"`
	Stderr string `json:"stderr,omitempty"`
}

// Recorder wraps a Runner and captures every result, keyed by command line.
// Captures from real hardware become test fixtures.
type Recorder struct {
	Inner Runner
	mu    sync.Mutex
	got   map[string]Recording
}

// Run implements Runner.
func (r *Recorder) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := r.Inner.Run(ctx, name, args...)
	rec := Recording{Stdout: string(out)}
	var ee *ExitError
	switch {
	case errors.As(err, &ee):
		rec.Exit, rec.Stderr = ee.Code, ee.Stderr
	case err != nil:
		rec.Exit, rec.Stderr = 127, err.Error()
	}
	r.mu.Lock()
	if r.got == nil {
		r.got = map[string]Recording{}
	}
	r.got[CmdLine(name, args...)] = rec
	r.mu.Unlock()
	return out, err
}

// Save writes the captured results as JSON.
func (r *Recorder) Save(path string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, err := json.MarshalIndent(r.got, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// Replay serves recorded results. Unrecorded commands fail with exit 127,
// as if the tool were missing.
type Replay map[string]Recording

// LoadReplay reads a fixture written by Recorder.Save.
func LoadReplay(path string) (Replay, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Replay
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return r, nil
}

// Run implements Runner.
func (r Replay) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	line := CmdLine(name, args...)
	rec, ok := r[line]
	if !ok {
		return nil, &ExitError{Cmd: line, Code: 127, Stderr: "not recorded"}
	}
	if rec.Exit != 0 {
		return []byte(rec.Stdout), &ExitError{Cmd: line, Code: rec.Exit, Stderr: rec.Stderr}
	}
	return []byte(rec.Stdout), nil
}
