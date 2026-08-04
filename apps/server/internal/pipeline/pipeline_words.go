package pipeline

import (
	"context"
	"fmt"
	"log"
	"strings"

	"buddy/server/internal/llm"
	"buddy/server/internal/protocol"
)

// SuggestWords asks the chat model for candidate English words/phrases
// matching a learner's native-language description of a word they can't
// recall mid-conversation (e.g. "화가 나서 참을 수 없는 느낌" -> "furious"/"livid").
// A fresh two-message call, same "never touches the session's own
// transcript" shape as GenerateTitle/analyze()-based tasks. Unlike the
// Analysis-ensemble tasks (GenerateStudySummary, correct(), ...), this uses a
// single fast call (p.LLM/p.ChatModel) — the same tier as GenerateTitle: it
// backs a live side panel a learner consults mid-typing, so latency matters
// more than the higher quality bar correct()/GenerateStudySummary need, and
// a mediocre suggestion just gets re-asked.
func (p *Pipeline) SuggestWords(ctx context.Context, description string) ([]protocol.WordSuggestion, error) {
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: wordSuggestionSystemPrompt(p.FeedbackLang)},
		{Role: llm.RoleUser, Content: description},
	}
	raw, err := p.LLM.Complete(ctx, p.ChatModel, msgs, true)
	if err != nil {
		return nil, err
	}
	parsed, err := parseJSON[struct {
		Suggestions []protocol.WordSuggestion `json:"suggestions"`
	}](raw, "word suggestion")
	if err != nil {
		return nil, err
	}
	return parsed.Suggestions, nil
}

// wordSuggestionSystemPrompt builds SuggestWords' prompt, reusing the same
// native-language config as correctionSystemPrompt/studySummarySystemPrompt.
func wordSuggestionSystemPrompt(lang string) string {
	native := languageName(lang)
	return fmt.Sprintf(`You help a %[1]s-speaking English learner who is mid-conversation and can't
recall an English word, by suggesting candidates from a %[1]s description of
what they mean.
Given a short %[1]s phrase describing a word or concept, suggest 3-5 English
word/phrase candidates that best match it, most likely match first.
Return STRICT JSON only, no prose, in exactly this shape:
{"suggestions":[{"word":"<English word or short phrase>","meaning":"<brief %[1]s gloss>","example":"<one example English sentence using it>"}]}
Rules:
- "word" MUST stay in English.
- "meaning" MUST be written in %[1]s.
- "example" MUST be a natural English sentence that uses "word".
- If the description is too vague to suggest anything meaningful, return an empty "suggestions" array rather than guessing wildly.`, native)
}

// autoAddSuggestionCount is how many candidates SuggestNewWords asks for —
// fixed (unlike SuggestWords' "3-5", which hedges against a vague
// description) since there's no learner query to hedge against here, and a
// predictable count keeps httpserver.wordAutoAddHandler's response size
// consistent from click to click.
const autoAddSuggestionCount = 5

// SuggestNewWords asks the chat model for new English word/phrase candidates
// to add to a learner's study list on their own initiative — the "새 단어
// 추가로 학습하기" button httpserver.wordAutoAddHandler backs, shown once
// WordReview.tsx's due queue is empty. Unlike SuggestWords, there's no
// learner-typed description to match: candidates are picked from
// learnerProfile (the same persistent, cross-session profile
// BuildSystemPrompt/UpdateLearnerProfile already use — interests,
// proficiency, recurring mistakes) so suggestions land near the learner's
// actual level instead of generic filler, and existingWords (the learner's
// own already-tracked words, any status) is passed so the model doesn't
// waste a pick re-suggesting something already on the list — real fact
// checking and any residual duplicate still goes through VerifyWord/Store.Save
// exactly like a manually picked word, this is just generation, same as
// SuggestWords.
func (p *Pipeline) SuggestNewWords(ctx context.Context, learnerProfile string, existingWords []string) ([]protocol.WordSuggestion, error) {
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: wordAutoSuggestSystemPrompt(p.FeedbackLang)},
		{Role: llm.RoleUser, Content: renderAutoSuggestInput(learnerProfile, existingWords)},
	}
	raw, err := p.LLM.Complete(ctx, p.ChatModel, msgs, true)
	if err != nil {
		return nil, err
	}
	parsed, err := parseJSON[struct {
		Suggestions []protocol.WordSuggestion `json:"suggestions"`
	}](raw, "word auto-suggestion")
	if err != nil {
		return nil, err
	}
	return parsed.Suggestions, nil
}

// wordAutoSuggestSystemPrompt builds SuggestNewWords' prompt, reusing the
// same native-language config as wordSuggestionSystemPrompt.
func wordAutoSuggestSystemPrompt(lang string) string {
	native := languageName(lang)
	return fmt.Sprintf(`You help a %[1]s-speaking English learner discover new English words/phrases
worth studying, on their own initiative (not in response to a specific
question they asked).
You will be given the learner's profile (their interests, proficiency level,
and recurring mistakes, in %[1]s — may be empty) and a list of words/phrases
they are already tracking, which you must NOT repeat.
Suggest exactly %[2]d new English word/phrase candidates that best fit this
learner's level and interests. If the profile is empty or too vague to infer
anything from, suggest common, generally useful intermediate-level words
instead of guessing wildly.
Return STRICT JSON only, no prose, in exactly this shape:
{"suggestions":[{"word":"<English word or short phrase>","meaning":"<brief %[1]s gloss>","example":"<one example English sentence using it>"}]}
Rules:
- "word" MUST stay in English, and MUST NOT be one already in the learner's tracked list.
- "meaning" MUST be written in %[1]s.
- "example" MUST be a natural English sentence that uses "word".`, native, autoAddSuggestionCount)
}

func renderAutoSuggestInput(learnerProfile string, existingWords []string) string {
	var b strings.Builder
	b.WriteString("Learner profile:\n")
	if learnerProfile == "" {
		b.WriteString("(none)\n")
	} else {
		b.WriteString(learnerProfile + "\n")
	}
	b.WriteString("\nAlready tracked (do not repeat):\n")
	if len(existingWords) == 0 {
		b.WriteString("(none)\n")
	} else {
		b.WriteString(strings.Join(existingWords, ", ") + "\n")
	}
	return b.String()
}

// minWordVerifyJudges is the minimum number of independent judgments
// VerifyWord collects before deciding — even a deployment with only one
// Analysis candidate configured (the common self-hosted single-local-model
// case, see README) still gets this many independent calls, round-robining
// through whatever candidates are configured, so "unanimous" always means
// more than one opinion, not just whatever the one available model said.
const minWordVerifyJudges = 3

// VerifyWord fact-checks one word/phrase the learner chose to study (see
// httpserver.wordSaveHandler, transport.WordVerifyJobHandler): is it a real,
// naturally used English word/phrase/idiom, does meaning accurately describe
// it, and does example use it correctly. Requires every judge to agree
// valid=true — a single dissent rejects the word immediately (with that
// judge's own reason) rather than being reconciled/outvoted the way
// analyze()'s Judge-synthesis step would; this is a fact-check, not a task
// where "the best merged answer" makes sense. Deliberately not a dictionary
// API lookup: this app assumes a local, possibly fully offline LLM (see
// README's llama.cpp/vLLM/LM Studio setup), so verification stays inside the
// same LLM infrastructure everything else here already depends on rather
// than adding a new external network dependency.
//
// Runs entirely in the background (see transport.WordVerifyJobHandler) —
// never on a request a learner is waiting on, since a local model can be
// slow and this makes minWordVerifyJudges calls, not one.
//
// Returns an error (not valid=false) if fewer than 2 judge calls succeeded —
// e.g. every configured endpoint is unreachable — so the caller leaves the
// word Pending for asyncjob's reaper to retry, instead of wrongly rejecting
// it over an infrastructure hiccup.
func (p *Pipeline) VerifyWord(ctx context.Context, word, meaning, example string) (valid bool, reason string, err error) {
	if len(p.Analysis) == 0 {
		return false, "", fmt.Errorf("verify word: no candidates configured")
	}
	n := minWordVerifyJudges
	if len(p.Analysis) > n {
		n = len(p.Analysis)
	}

	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: wordVerifySystemPrompt(p.FeedbackLang)},
		{Role: llm.RoleUser, Content: fmt.Sprintf("word: %s\nmeaning: %s\nexample: %s", word, meaning, example)},
	}

	type verdict struct {
		valid  bool
		reason string
		ok     bool
	}
	// Round-robins through p.Analysis (i % len(p.Analysis)) rather than
	// indexing directly, since n can exceed len(p.Analysis) — see
	// minWordVerifyJudges.
	results := fanOutOrdered(n, func(i int) verdict {
		c := p.Analysis[i%len(p.Analysis)]
		text, err := c.LLM.Complete(ctx, c.Model, msgs, true)
		if err != nil {
			log.Printf("verify word: candidate %s: %v", c.Model, err)
			return verdict{}
		}
		parsed, err := parseJSON[struct {
			Valid  bool   `json:"valid"`
			Reason string `json:"reason"`
		}](text, "verify word: candidate "+c.Model)
		if err != nil {
			log.Printf("%v", err)
			return verdict{}
		}
		return verdict{valid: parsed.Valid, reason: parsed.Reason, ok: true}
	})

	succeeded := 0
	for _, r := range results {
		if !r.ok {
			continue
		}
		succeeded++
		if !r.valid {
			return false, r.reason, nil
		}
	}
	if succeeded < 2 {
		return false, "", fmt.Errorf("verify word: only %d/%d judge calls succeeded", succeeded, n)
	}
	return true, "", nil
}

// wordVerifySystemPrompt builds VerifyWord's fact-checking prompt, reusing
// the same native-language config as wordSuggestionSystemPrompt.
func wordVerifySystemPrompt(lang string) string {
	native := languageName(lang)
	return fmt.Sprintf(`You are a strict fact-checker reviewing one entry a %[1]s-speaking English
learner wants to add to their vocabulary study list.
Given "word" (an English word, short phrase, or idiom), "meaning" (a %[1]s
gloss someone claims for it), and "example" (an English sentence claiming to
use it), judge ALL of the following:
- "word" is a real, naturally used English word, phrase, or idiom — not a
  typo, nonsense string, or invented term.
- "meaning" accurately and specifically describes "word" in %[1]s — not
  vague, not wrong, not another word's meaning.
- "example" is a natural, grammatically correct English sentence that
  actually uses "word" the way "meaning" describes.
Return STRICT JSON only, no prose, in exactly this shape:
{"valid":true|false,"reason":"<one short %[1]s sentence explaining your verdict, especially if false>"}`, native)
}
