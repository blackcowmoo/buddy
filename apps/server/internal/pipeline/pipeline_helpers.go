package pipeline

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"buddy/server/internal/llm"
)

// parseJSON unmarshals raw into a fresh T, wrapping a decode error as
// "<label>: bad json: %w" — or just "bad json: %w" when label is "" — the
// shared shape behind parseCorrection, GenerateStudySummary,
// GenerateStudyQuiz, CheckQuizAnswer, SuggestWords, and VerifyWord's
// per-candidate parse, which otherwise each hand-roll the same
// json.Unmarshal-and-wrap.
func parseJSON[T any](raw, label string) (T, error) {
	var v T
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		if label == "" {
			return v, fmt.Errorf("bad json: %w", err)
		}
		return v, fmt.Errorf("%s: bad json: %w", label, err)
	}
	return v, nil
}

// fanOutOrdered runs call(0), call(1), ..., call(n-1) concurrently and
// returns their results in that same index order rather than completion
// order — both transcribe (STT engines) and analyze (LLM candidates) rely
// on "results[0]" meaning "the first configured engine/candidate" for their
// first-candidate fallback, regardless of which goroutine finishes first.
func fanOutOrdered[T any](n int, call func(i int) T) []T {
	results := make([]T, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = call(i)
		}(i)
	}
	wg.Wait()
	return results
}

// writeTranscript appends each message as a "role: content" line to b — the
// shared rendering used everywhere an LLM prompt needs to show msgs as prior
// conversation (transcript synthesis, compaction, translation/correction
// context).
func writeTranscript(b *strings.Builder, msgs []llm.Message) {
	for _, m := range msgs {
		fmt.Fprintf(b, "%s: %s\n", m.Role, m.Content)
	}
}

// languageName maps a short language code to an English name the LLM
// understands. Unknown codes fall back to the code itself.
func languageName(code string) string {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "ko", "ko-kr":
		return "Korean"
	case "en", "en-us":
		return "English"
	case "ja", "ja-jp":
		return "Japanese"
	case "zh", "zh-cn":
		return "Chinese"
	case "es":
		return "Spanish"
	case "", "auto":
		return "Korean"
	default:
		return code
	}
}
