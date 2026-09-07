package transport

import (
	"context"
	"log"
	"strings"
	"unicode/utf8"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/protocol"
	"buddy/server/internal/wordreview"
)

// maxCapturedWordLen/maxCapturedFieldLen mirror httpserver's
// maxWordLen/maxWordFieldLen — the same buddy_word_reviews column limits —
// since captureCorrectionWords bypasses wordSaveHandler's own request-body
// validation and writes to wordreview.Store directly.
const (
	maxCapturedWordLen  = 255
	maxCapturedFieldLen = 2000
)

// captureCorrectionWords adds one conversation turn's vocabulary/phrasing
// corrections to the learner's spaced-repetition study list, alongside
// whatever they've explicitly searched and saved via the word-search popup
// (see wordreview's package doc). The idea: a word the learner reached for
// mid-conversation and got wrong is just as worth drilling as one they
// couldn't produce at all — maybe more so, since they clearly know *of* it
// already and just need the recall reinforced.
//
// Only Issue.Type "vocabulary" and "phrasing" qualify — grammar/context
// issues are about sentence structure, not a word/phrase worth adding to a
// vocabulary list. Each qualifying issue maps onto wordreview.Store.Save's
// (word, meaning, example) shape the same way pipeline.SuggestWords'
// WordSuggestion does: Issue.Suggestion (the corrected expression) as the
// word, Issue.ExplanationTranslation (already a native-language gloss, same
// role as WordSuggestion.Meaning) as the meaning, and the full
// Correction.Corrected sentence — already a natural sentence using the
// fixed expression — as the example.
//
// A captured word starts StatusPending and goes through the exact same
// VerifyWord Chat -> Analysis -> Judge check as a manually saved one (see
// wordSaveHandler) before it ever becomes due for review — this never
// bypasses that quality bar just because it originated from a correction.
// words.Save's (userID, word, meaning) dedup makes this safe to call
// repeatedly for what's conceptually the same correction (the Chat preview
// and Judge final, or the queued and live persistence paths, can
// each independently call this for the same turn) — a repeat just returns
// the existing row instead of duplicating it.
func captureCorrectionWords(ctx context.Context, pipe *pipeline.Pipeline, words wordreview.Store, wordVerifyQueue *asyncjob.Queue, userID string, c protocol.Correction) {
	if words == nil {
		return
	}
	example := strings.TrimSpace(c.Corrected)
	for _, issue := range c.Issues {
		if issue.Type != "vocabulary" && issue.Type != "phrasing" {
			continue
		}
		word := strings.TrimSpace(issue.Suggestion)
		if word == "" || strings.EqualFold(word, strings.TrimSpace(issue.Span)) {
			continue // nothing actually corrected here
		}
		meaning := strings.TrimSpace(issue.ExplanationTranslation)
		if utf8.RuneCountInString(word) > maxCapturedWordLen ||
			utf8.RuneCountInString(meaning) > maxCapturedFieldLen ||
			utf8.RuneCountInString(example) > maxCapturedFieldLen {
			continue // wildly oversized LLM output isn't worth failing the correction save over
		}

		saved, err := words.Save(ctx, userID, word, meaning, example)
		if err != nil {
			log.Printf("word capture: save %s: %v", userID, err)
			continue
		}
		if saved.Status != wordreview.StatusPending {
			continue // already decided (verified/rejected) by an earlier capture of this same word+meaning
		}

		if wordVerifyQueue != nil {
			if err := EnqueueWordVerifyJob(ctx, wordVerifyQueue, pipe, words, userID, saved.ID); err == nil {
				continue
			}
		}
		go func(wordID string) {
			if err := RunWordVerifyInline(context.Background(), pipe, words, userID, wordID); err != nil {
				log.Printf("word capture: verify %s/%s: %v", userID, wordID, err)
			}
		}(saved.ID)
	}
}
