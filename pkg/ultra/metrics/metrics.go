// Package metrics records each ULTRA execution attempt so its cost and
// outcome can be compared: executor tokens (reported by the provider or
// estimated, never mixed), background model use, injected context size,
// coordination and validation time, retries and the accepted outcome.
package metrics

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Attempt is one executor attempt and what the coordinator made of it.
type Attempt struct {
	Task      string    `json:"task"`
	Attempt   int       `json:"attempt"`
	Mode      string    `json:"mode"`
	Executor  string    `json:"executor"`
	Model     string    `json:"model,omitempty"`
	ConfigRev int       `json:"config_revision"`
	Started   time.Time `json:"started"`
	Ended     time.Time `json:"ended"`
	// Times in milliseconds.
	TotalMS        int64 `json:"total_ms"`
	ExecutorMS     int64 `json:"executor_ms"`
	ValidationMS   int64 `json:"validation_ms"`
	CoordinationMS int64 `json:"coordination_ms"` // total less executor and validation
	ContextMS      int64 `json:"context_ms"`      // selecting the context, part of coordination
	// Context PAI injected: an estimate.
	ContextTokensEst int  `json:"context_tokens_est"`
	ContextOverrun   bool `json:"context_overrun"`
	// Executor usage. Reported is true when the provider reported it.
	InputTokens     int  `json:"input_tokens"`
	OutputTokens    int  `json:"output_tokens"`
	CacheReadTokens int  `json:"cache_read_tokens"`
	UsageReported   bool `json:"usage_reported"`
	// Model calls PAI made outside the executor (extraction, grading,
	// summaries, routing). The lightweight profile makes none.
	BackgroundCalls  int `json:"background_calls"`
	BackgroundTokens int `json:"background_tokens"`
	// Remote round trips PAI itself made to a server for this attempt.
	RemoteCalls int `json:"remote_calls"`
	// Outcome.
	NativeResume       bool   `json:"native_resume"`
	Resumed            bool   `json:"resumed"`
	ExecutorClaim      string `json:"executor_claim"`
	ValidationPassed   bool   `json:"validation_passed"`
	ValidationFailures int    `json:"validation_failures"`
	Outcome            string `json:"outcome"` // accepted, retry, blocked, failed, cancelled, error
	Error              string `json:"error,omitempty"`
}

// Log is an append-only JSONL file of attempts.
type Log struct {
	Path string
	mu   sync.Mutex
}

// Append writes one attempt.
func (l *Log) Append(a Attempt) error {
	if l == nil || l.Path == "" {
		return nil
	}
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(l.Path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(l.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// Read returns every attempt, oldest first.
func (l *Log) Read() ([]Attempt, error) {
	f, err := os.Open(l.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Attempt
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		var a Attempt
		if json.Unmarshal(sc.Bytes(), &a) == nil {
			out = append(out, a)
		}
	}
	return out, sc.Err()
}

// Summary totals a set of attempts.
type Summary struct {
	Attempts           int   `json:"attempts"`
	Tasks              int   `json:"tasks"`
	Accepted           int   `json:"accepted"`
	Retries            int   `json:"retries"`
	ValidationFailures int   `json:"validation_failures"`
	ReportedTokens     int   `json:"reported_tokens"`
	UnreportedAttempts int   `json:"unreported_attempts"` // attempts whose executor did not report usage
	ContextTokensEst   int   `json:"context_tokens_est"`
	BackgroundCalls    int   `json:"background_calls"`
	BackgroundTokens   int   `json:"background_tokens"`
	ExecutorMS         int64 `json:"executor_ms"`
	ValidationMS       int64 `json:"validation_ms"`
	CoordinationMS     int64 `json:"coordination_ms"`
}

// Summarize totals attempts.
func Summarize(as []Attempt) Summary {
	var s Summary
	tasks := map[string]bool{}
	for _, a := range as {
		s.Attempts++
		tasks[a.Task] = true
		if a.Outcome == "accepted" {
			s.Accepted++
		}
		if a.Attempt > 1 {
			s.Retries++
		}
		s.ValidationFailures += a.ValidationFailures
		if a.UsageReported {
			s.ReportedTokens += a.InputTokens + a.OutputTokens + a.CacheReadTokens
		} else {
			s.UnreportedAttempts++
		}
		s.ContextTokensEst += a.ContextTokensEst
		s.BackgroundCalls += a.BackgroundCalls
		s.BackgroundTokens += a.BackgroundTokens
		s.ExecutorMS += a.ExecutorMS
		s.ValidationMS += a.ValidationMS
		s.CoordinationMS += a.CoordinationMS
	}
	s.Tasks = len(tasks)
	return s
}
