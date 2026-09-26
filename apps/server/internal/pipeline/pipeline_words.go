package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"buddy/server/internal/llm"
	"buddy/server/internal/protocol"
	"buddy/server/internal/wordreview"
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
	raw, err := p.complete(ctx, p.LLM, p.ChatModel, msgs, true)
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
- "word" MUST be lowercase, except the pronoun "I" and its contractions
  ("I'm", "I'll", "I'd", "I've") when they are part of the word/phrase.
%[2]s
- "example" MUST be a natural English sentence that uses "word".
- If the description is too vague to suggest anything meaningful, return an empty "suggestions" array rather than guessing wildly.`, native, dictionaryMeaningRules(native))
}

// All vocabulary sources share the same flashcard contract. Context selects
// a dictionary sense; the gloss must remain useful outside the source passage.
func dictionaryMeaningRules(native string) string {
	rules := fmt.Sprintf(`- "meaning" MUST be written in %[1]s as a concise, dictionary-style translation
  of the English word/phrase in the intended sense. It must stand on its own
  beside the English entry as the answer the learner should memorize.
- Use context to select ONE dictionary sense and part of speech. Give one to
  three short equivalents within that sense; do not mix unrelated senses.
  Use base dictionary wording, not the sentence's tense or named referents.
- Preserve essential distinctions and grammatical complements. A brief sense
  qualifier is allowed when necessary to distinguish meanings, but omit
  redundant explanations, technical background, and long parenthetical notes.
- "meaning" MUST NOT contain metacommentary such as "in this context", "here",
  "means", "refers to", or "is used to", or their %[1]s equivalents.
- "meaning" MUST NOT contain an extra English definition, an example,
  etymology, a usage note, or a translation of the full sentence.
- When listing several senses, put each distinct sense in a separate entry.
  Examples below illustrate individual entries, not the number to return.`, native)
	if native == "Korean" {
		rules += `
Korean dictionary-gloss examples (follow the requested JSON wrapper):
Context: "The company closed its steel facility."
Good: {"word":"facility","meaning":"생산 시설","example":"The company closed its steel facility."}
Bad meaning: "맥락상 공장이나 설비, 또는 운영 장소를 의미함"
Context: "The jury convicted him of fraud."
Good: {"word":"convict","meaning":"유죄 판결을 내리다","example":"The jury convicted him of fraud."}
Bad meaning: "유죄 판결을 선고하다 (To find someone guilty of a crime)"
Context: "The bridge is made of steel."
Good: {"word":"steel","meaning":"강철, 철강","example":"The bridge is made of steel."}
Bad meaning: "강철 또는 철강 (철과 탄소를 가열하여 만든 산업용 금속 재료)"
Context: "The shop will close next month."
Good meaning for close: "문을 닫다, 영업을 중단하다"
Context: "They are close friends."
Good meaning for close: "친한, 가까운" (a separate sense, not mixed with "닫다")
Context: "Despite the rain, we left."
Good meaning for despite: "~에도 불구하고"
Context: "The country faces a budget deficit."
Good meaning for deficit: "재정 적자"
Context: "She runs the company."
Good meaning for run: "경영하다, 운영하다" (not "달리다" or "그녀는 회사를 운영한다")
Context: "They put off the meeting."
Good meaning for put off: "미루다, 연기하다"
Keep these examples' concise style; do not copy their words or meanings when
the input refers to a different word or sense.`
	}
	return rules
}

// GenerateWordReviewQuestion turns one verified dictionary entry into the
// exact recall question shown by WordReview. Each answer is intentionally the
// surface form required by its blank, not necessarily the stored base Word:
// learners should supply "organized" when the sentence requires past tense,
// rather than type "organize" into a partially hidden "___d". Grammar-only
// placeholders in dictionary phrases stay visible: "do one's best" produces
// "___ his ___" and tests only "do" and "best".
//
// Questions are versioned in application code rather than trusted to the
// model. The caller persists this value with the prompt and answers so legacy
// questions can be regenerated and stale workers can be rejected atomically.
func (p *Pipeline) GenerateWordReviewQuestion(ctx context.Context, word, meaning, example string) (wordreview.Question, error) {
	raw, err := p.analyze(ctx, wordReviewQuestionSystemPrompt(p.FeedbackLang), fmt.Sprintf("dictionary word: %s\nmeaning: %s\nexisting example: %s", word, meaning, example), true)
	if err != nil {
		return wordreview.Question{}, err
	}
	parsed, err := parseJSON[struct {
		Prompt  string   `json:"prompt"`
		Answers []string `json:"answers"`
	}](raw, "word review question")
	if err != nil {
		return wordreview.Question{}, err
	}
	prompt := strings.TrimSpace(parsed.Prompt)
	answers := make([]string, len(parsed.Answers))
	for i, answer := range parsed.Answers {
		answers[i] = strings.TrimSpace(answer)
	}
	parts := strings.Split(prompt, "___")
	if len(answers) == 0 || len(parts) != len(answers)+1 {
		return wordreview.Question{}, fmt.Errorf("word review question: prompt blanks must match answers")
	}
	for i, answer := range answers {
		before, after := []rune(parts[i]), []rune(parts[i+1])
		if (len(before) > 0 && isWordFormRune(before[len(before)-1])) || (len(after) > 0 && isWordFormRune(after[0])) {
			return wordreview.Question{}, fmt.Errorf("word review question: blank must replace a complete grammatical form")
		}
		if answer == "" {
			return wordreview.Question{}, fmt.Errorf("word review question: answer is empty")
		}
	}
	if expectedCounts, hasPlaceholder := placeholderSeparatedWordCounts(word); hasPlaceholder {
		if len(answers) != len(expectedCounts) {
			return wordreview.Question{}, fmt.Errorf("word review question: grammar placeholders must remain visible")
		}
		for i, expected := range expectedCounts {
			if len(strings.Fields(answers[i])) != expected {
				return wordreview.Question{}, fmt.Errorf("word review question: answer %d includes context-only words", i+1)
			}
		}
	} else if len(answers) != 1 {
		return wordreview.Question{}, fmt.Errorf("word review question: ordinary entries require one blank")
	}
	encodedAnswers, err := json.Marshal(answers)
	if err != nil || len([]rune(string(encodedAnswers))) > 255 {
		return wordreview.Question{}, fmt.Errorf("word review question: answers exceed 255 characters")
	}
	question := wordreview.Question{Version: wordreview.CurrentQuestionVersion, Prompt: prompt, Answers: answers}
	if len(answers) == 1 {
		question.Answer = answers[0]
	}
	return question, nil
}

// placeholderSeparatedWordCounts returns the number of lexical words on each
// side of a dictionary grammar placeholder. For example, "do one's best"
// becomes [1, 1]. Matching generated answer lengths to these segments keeps
// the context-selected possessive out of the learner's blanks while still
// allowing lexical inflection ("make" -> "made").
func placeholderSeparatedWordCounts(word string) ([]int, bool) {
	placeholders := map[string]bool{
		"one's": true, "oneself": true,
		"someone": true, "someone's": true, "somebody": true, "somebody's": true,
		"something": true, "something's": true,
	}
	counts := []int{}
	count := 0
	found := false
	for _, raw := range strings.Fields(strings.ReplaceAll(strings.ToLower(word), "’", "'")) {
		token := strings.Trim(raw, `.,;:!?()[]{}"`)
		if placeholders[token] {
			found = true
			if count > 0 {
				counts = append(counts, count)
				count = 0
			}
			continue
		}
		count++
	}
	if count > 0 {
		counts = append(counts, count)
	}
	return counts, found
}

func isWordFormRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '\'' || r == '’' || r == '-'
}

func wordReviewQuestionSystemPrompt(lang string) string {
	native := languageName(lang)
	return fmt.Sprintf(`You create one fill-in-the-blank recall question for a %[1]s-speaking
English learner from a verified vocabulary entry.
Return STRICT JSON only, no prose, in exactly this shape:
{"prompt":"<one natural English sentence with one or more ___ blanks>","answers":["<exact text for the first blank>"]}
Rules:
- Test the supplied dictionary word/phrase in the supplied meaning.
- The sentence must make the required grammatical form clear from context.
- Each item in "answers" MUST use the complete grammatical form required by
  its corresponding blank, including tense, aspect, subject agreement, or
  number. It does NOT have to equal the dictionary form. For example, use
  "organized" rather than "organize" when the sentence is in the past.
- Replace each complete lexical answer with "___". Never leave an inflectional
  suffix visible outside the blank (wrong: "___d" for "organized").
- Dictionary placeholders such as "one's", "someone", "something", or
  "oneself" are grammar context, not vocabulary to test. Leave their natural
  sentence-specific replacement visible and blank only the lexical parts on
  either side. Example: "do one's best" with subject "he" MUST be
  "He promised to ___ his ___." with answers ["do","best"], never one blank
  whose answer is "do his best" and never a blank for "his".
- Use one blank for an ordinary word or phrase with no dictionary placeholder.
- The completed sentence must be natural and unambiguous. You may rewrite the
  existing example when it does not satisfy these rules.
- Keep the prompt and every answer in English.`, native)
}

// DefineWord is the common word-search entry point. It first uses the fast
// chat model to turn an inflected spelling into its dictionary form, then
// defines that form in context. If either the form-resolution call or the
// lookup using the resolved form fails, it retries the lookup with the exact
// spelling the learner supplied. Keeping this fallback here (rather than in
// HTTP handlers or jobs) makes every word-search caller use the same policy.
func (p *Pipeline) DefineWord(ctx context.Context, word, passage string) (protocol.WordSuggestion, error) {
	word = strings.TrimSpace(word)
	resolved, err := p.resolveWordForm(ctx, word, passage)
	if err == nil && resolved != "" {
		result, lookupErr := p.defineWord(ctx, resolved, passage)
		if lookupErr == nil {
			return result, nil
		}
		// The model may have proposed a form that the definition call cannot
		// handle. Re-run the same lookup with the learner's original input.
		if fallback, fallbackErr := p.defineWord(ctx, word, passage); fallbackErr == nil {
			return fallback, nil
		} else {
			return protocol.WordSuggestion{}, fmt.Errorf("word definition: resolved lookup: %w; original lookup: %v", lookupErr, fallbackErr)
		}
	}

	// Resolution is deliberately best-effort: an unavailable or malformed
	// fast-model response must not prevent the ordinary word lookup.
	return p.defineWord(ctx, word, passage)
}

func (p *Pipeline) DefineWordMeanings(ctx context.Context, word, passage string) ([]protocol.WordSuggestion, error) {
	word = strings.TrimSpace(word)
	lookupWord := word
	if resolved, err := p.resolveWordForm(ctx, word, passage); err == nil && resolved != "" {
		lookupWord = resolved
	}
	results, err := p.defineWordMeanings(ctx, lookupWord, passage)
	if err == nil || lookupWord == word {
		return results, err
	}
	// Keep the research feature useful if the form resolver produced a form
	// the dictionary model cannot handle: retry with the exact spelling.
	return p.defineWordMeanings(ctx, word, passage)
}

func (p *Pipeline) defineWordMeanings(ctx context.Context, word, passage string) ([]protocol.WordSuggestion, error) {
	native := languageName(p.FeedbackLang)
	systemPrompt := fmt.Sprintf(`You are a dictionary assistant for a %[1]s-speaking English learner. List up to 6 distinct common meanings of the given English word or phrase in its base dictionary form; do not invent senses to fill a quota. Put the context-matching sense first. Return STRICT JSON only: {"suggestions":[{"word":"...","meaning":"...","example":"..."}]}. Keep word in English and give one natural English example for every meaning.
%[2]s`, native, dictionaryMeaningRules(native))
	raw, err := p.analyze(ctx, systemPrompt, fmt.Sprintf("word: %s\ncontext: %s", word, passage), true)
	if err != nil {
		return nil, err
	}
	parsed, err := parseJSON[struct {
		Suggestions []protocol.WordSuggestion `json:"suggestions"`
	}](raw, "word meanings")
	if err != nil {
		return nil, err
	}
	return parsed.Suggestions, nil
}

func (p *Pipeline) resolveWordForm(ctx context.Context, word, passage string) (string, error) {
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: wordFormSystemPrompt()},
		{Role: llm.RoleUser, Content: fmt.Sprintf("word: %s\ncontext: %s", word, passage)},
	}
	raw, err := p.complete(ctx, p.LLM, p.ChatModel, msgs, true)
	if err != nil {
		return "", err
	}
	parsed, err := parseJSON[struct {
		Word string `json:"word"`
	}](raw, "word form")
	if err != nil {
		return "", err
	}
	resolved := strings.TrimSpace(parsed.Word)
	if resolved == "" {
		return "", fmt.Errorf("word form: model returned an empty word")
	}
	return resolved, nil
}

func (p *Pipeline) defineWord(ctx context.Context, word, passage string) (protocol.WordSuggestion, error) {
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: wordDefineSystemPrompt(p.FeedbackLang)},
		{Role: llm.RoleUser, Content: fmt.Sprintf("word: %s\ncontext: %s", word, passage)},
	}
	raw, err := p.complete(ctx, p.LLM, p.ChatModel, msgs, true)
	if err != nil {
		return protocol.WordSuggestion{}, err
	}
	parsed, err := parseJSON[protocol.WordSuggestion](raw, "word definition")
	if err != nil {
		return protocol.WordSuggestion{}, err
	}
	return parsed, nil
}

func wordFormSystemPrompt() string {
	return `You normalize one English word or short phrase for a dictionary lookup.
Given the exact word a learner selected and the context where it appeared,
return its base dictionary form if it is inflected (for example "running" ->
"run", "better" -> "good", "children" -> "child"). Preserve the input when
it is already the appropriate dictionary form. Return STRICT JSON only, with
exactly this shape:
{"word":"<dictionary form>"}
Rules:
- Keep the result in English.
- Return only one word or short phrase, with no explanation.
- Do not translate, define, or add punctuation.`
}

// wordDefineSystemPrompt builds DefineWord's prompt, reusing the same
// native-language config as wordSuggestionSystemPrompt.
func wordDefineSystemPrompt(lang string) string {
	native := languageName(lang)
	return fmt.Sprintf(`You help a %[1]s-speaking English learner who tapped an English word or
short phrase while reading, because they don't know it.
You will be given "word" (the exact English word/phrase they tapped) and
"context" (the passage it appeared in). Define "word" as it is actually used
in that context.
Return STRICT JSON only, no prose, in exactly this shape:
{"word":"<the word/phrase, corrected to its dictionary form if "word" was an inflected form found in "context">","meaning":"<brief %[1]s gloss, specific to how it's used in "context">","example":"<one example English sentence using "word", may reuse the sentence from "context">"}
Rules:
- "word" MUST stay in English.
- "word" MUST be lowercase, except the pronoun "I" and its contractions
  ("I'm", "I'll", "I'd", "I've") when they are part of the word/phrase.
%[2]s
- "example" MUST be a natural English sentence that uses "word".`, native, dictionaryMeaningRules(native))
}

// autoAddSuggestionCount is how many candidates SuggestNewWords asks for —
// fixed (unlike SuggestWords' "3-5", which hedges against a vague
// description) since there's no learner query to hedge against here, and a
// predictable count keeps httpserver.wordAutoAddHandler's response size
// consistent from click to click.
const autoAddSuggestionCount = 5

// SuggestNewWords generates new English word/phrase candidates through the
// full ordered cascade, to add to a learner's study list on their own initiative — the "새 단어
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
// exactly like a manually picked word. The draft stays internal; only the
// Judge result is returned.
func (p *Pipeline) SuggestNewWords(ctx context.Context, learnerProfile string, existingWords []string) ([]protocol.WordSuggestion, error) {
	raw, err := p.analyze(ctx, wordAutoSuggestSystemPrompt(p.FeedbackLang), renderAutoSuggestInput(learnerProfile, existingWords), true)
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
- "word" MUST be lowercase, except the pronoun "I" and its contractions
  ("I'm", "I'll", "I'd", "I've") when they are part of the word/phrase.
%[3]s
- "example" MUST be a natural English sentence that uses "word".`, native, autoAddSuggestionCount, dictionaryMeaningRules(native))
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

// VerifyWord fact-checks one word/phrase the learner chose to study (see
// httpserver.wordSaveHandler, transport.WordVerifyJobHandler). It uses the
// same Chat -> Analysis -> Judge cascade as every other learning decision:
// Chat supplies the first verdict, Analysis models refine it, and Judge owns
// the final boolean/reason after seeing the full chain as evidence.
//
// Runs entirely in the background (see transport.WordVerifyJobHandler) —
// never on a request a learner is waiting on, since a local model can be
// slow and this makes several ordered model calls, not one.
func (p *Pipeline) VerifyWord(ctx context.Context, word, meaning, example string) (valid bool, reason string, err error) {
	raw, err := p.analyze(ctx, wordVerifySystemPrompt(p.FeedbackLang), fmt.Sprintf("word: %s\nmeaning: %s\nexample: %s", word, meaning, example), true)
	if err != nil {
		return false, "", err
	}
	parsed, err := parseJSON[struct {
		Valid  *bool  `json:"valid"`
		Reason string `json:"reason"`
	}](raw, "verify word")
	if err != nil {
		return false, "", err
	}
	if parsed.Valid == nil {
		return false, "", fmt.Errorf("verify word: response is missing the valid verdict")
	}
	reason = strings.TrimSpace(parsed.Reason)
	if !*parsed.Valid && reason == "" {
		return false, "", fmt.Errorf("verify word: rejected response is missing a reason")
	}
	return *parsed.Valid, reason, nil
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
