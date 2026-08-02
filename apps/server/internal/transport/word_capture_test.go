package transport

import (
	"context"
	"strings"
	"testing"
	"time"

	"buddy/server/internal/pipeline"
	"buddy/server/internal/protocol"
	"buddy/server/internal/wordreview"
)

// TestCaptureCorrectionWordsSavesVocabularyAndPhrasingIssues guards the core
// scope decision: vocabulary and phrasing fixes join the study list, grammar
// fixes (about sentence structure, not a word/phrase) don't.
func TestCaptureCorrectionWordsSavesVocabularyAndPhrasingIssues(t *testing.T) {
	pipe := &pipeline.Pipeline{Analysis: []pipeline.Candidate{{Model: "m", LLM: fakeAnalysisLLM{complete: `{"valid":true,"reason":""}`}}}}
	words := newFakeWordReviewStore()
	c := protocol.Correction{
		Corrected: "She was furious about the delay.",
		Issues: []protocol.Issue{
			{Type: "vocabulary", Span: "very angry", Suggestion: "furious", ExplanationTranslation: "몹시 화난"},
			{Type: "phrasing", Span: "angry about", Suggestion: "furious about", ExplanationTranslation: "더 자연스러운 표현"},
			{Type: "grammar", Span: "was", Suggestion: "were", ExplanationTranslation: "should be skipped"},
		},
	}

	captureCorrectionWords(context.Background(), pipe, words, nil, "alex", c)

	list, err := words.List(context.Background(), "alex")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List() = %+v, want exactly 2 captured words (vocabulary + phrasing, not grammar)", list)
	}
	var gotWords []string
	for _, w := range list {
		gotWords = append(gotWords, w.Word)
		if w.Example != c.Corrected {
			t.Errorf("word %q example = %q, want the corrected sentence %q", w.Word, w.Example, c.Corrected)
		}
	}
	if !strings.Contains(strings.Join(gotWords, ","), "furious") {
		t.Fatalf("captured words = %v, want to include the vocabulary/phrasing suggestions", gotWords)
	}
}

// TestCaptureCorrectionWordsSkipsNoOpAndEmptySuggestions guards against
// polluting the study list with issues that don't actually name a fixed
// word/phrase.
func TestCaptureCorrectionWordsSkipsNoOpAndEmptySuggestions(t *testing.T) {
	pipe := &pipeline.Pipeline{}
	words := newFakeWordReviewStore()
	c := protocol.Correction{
		Corrected: "Whatever.",
		Issues: []protocol.Issue{
			{Type: "vocabulary", Span: "furious", Suggestion: "furious"}, // no actual change
			{Type: "vocabulary", Span: "furious", Suggestion: "  "},      // effectively empty
		},
	}

	captureCorrectionWords(context.Background(), pipe, words, nil, "alex", c)

	list, err := words.List(context.Background(), "alex")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("List() = %+v, want none captured", list)
	}
}

// TestCaptureCorrectionWordsSkipsOversizedFields guards the
// buddy_word_reviews column-length limits (word VARCHAR(255)) against
// unexpectedly long LLM output, without letting that fail the correction
// save itself.
func TestCaptureCorrectionWordsSkipsOversizedFields(t *testing.T) {
	pipe := &pipeline.Pipeline{}
	words := newFakeWordReviewStore()
	c := protocol.Correction{
		Corrected: "Whatever.",
		Issues: []protocol.Issue{
			{Type: "vocabulary", Span: "x", Suggestion: strings.Repeat("a", maxCapturedWordLen+1)},
		},
	}

	captureCorrectionWords(context.Background(), pipe, words, nil, "alex", c)

	list, err := words.List(context.Background(), "alex")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("List() = %+v, want the oversized suggestion skipped", list)
	}
}

// TestCaptureCorrectionWordsDedupesRepeatedCalls mirrors the real reason this
// matters: the fast and ensemble analysis passes (and the queued/live
// persistence paths) can each independently call this for what's
// conceptually the same correction — it must not create duplicate rows.
func TestCaptureCorrectionWordsDedupesRepeatedCalls(t *testing.T) {
	pipe := &pipeline.Pipeline{}
	words := newFakeWordReviewStore()
	c := protocol.Correction{
		Corrected: "She was furious about the delay.",
		Issues: []protocol.Issue{
			{Type: "vocabulary", Span: "very angry", Suggestion: "furious", ExplanationTranslation: "몹시 화난"},
		},
	}

	captureCorrectionWords(context.Background(), pipe, words, nil, "alex", c)
	captureCorrectionWords(context.Background(), pipe, words, nil, "alex", c)

	list, err := words.List(context.Background(), "alex")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List() = %+v, want exactly 1 row after two identical captures", list)
	}
}

// TestCaptureCorrectionWordsNilStoreIsNoop guards deployments/tests that
// don't wire a wordreview.Store through — must never panic.
func TestCaptureCorrectionWordsNilStoreIsNoop(t *testing.T) {
	pipe := &pipeline.Pipeline{}
	c := protocol.Correction{Issues: []protocol.Issue{{Type: "vocabulary", Span: "a", Suggestion: "b"}}}
	captureCorrectionWords(context.Background(), pipe, nil, nil, "alex", c)
}

// TestCaptureCorrectionWordsTriggersVerification guards that a captured word
// goes through the exact same VerifyWord model-consensus check a manually
// saved word does, never straight to Verified.
func TestCaptureCorrectionWordsTriggersVerification(t *testing.T) {
	pipe := &pipeline.Pipeline{Analysis: []pipeline.Candidate{{Model: "m", LLM: fakeAnalysisLLM{complete: `{"valid":true,"reason":""}`}}}}
	words := newFakeWordReviewStore()
	c := protocol.Correction{
		Corrected: "She was furious about the delay.",
		Issues:    []protocol.Issue{{Type: "vocabulary", Span: "very angry", Suggestion: "furious", ExplanationTranslation: "몹시 화난"}},
	}

	captureCorrectionWords(context.Background(), pipe, words, nil, "alex", c)

	list, err := words.List(context.Background(), "alex")
	if err != nil || len(list) != 1 {
		t.Fatalf("List() = %+v, %v, want exactly 1 captured word", list, err)
	}
	id := list[0].ID
	waitForCondition(t, 2*time.Second, func() bool {
		return words.status(id) == wordreview.StatusVerified
	})
}
