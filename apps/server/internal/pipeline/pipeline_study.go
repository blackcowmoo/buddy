package pipeline

import (
	"context"
	"fmt"
	"strings"

	"buddy/server/internal/protocol"
)

// StudyIssue pairs one flagged grammar/vocabulary/phrasing/context issue
// with the learner's original sentence it came from — the raw material
// GenerateStudySummary synthesizes into a session-wide "what to study next"
// recommendation. Callers assemble this from a session's persisted
// corrections (see httpserver.sessionStudySummaryHandler); Pipeline itself
// tracks nothing session-wide.
type StudyIssue struct {
	Text  string
	Issue protocol.Issue
}

// GenerateStudySummary synthesizes every grammar/vocabulary/phrasing/context
// issue flagged across a session into one encouraging wrap-up: the recurring
// PATTERNS a learner should focus on studying next, rather than a re-listing
// of each individual correction (already visible via the frontend's
// FeedbackSummary/GrammarControl). Written in English — the language being
// learned — sentence by sentence, each paired with a native-language
// translation, the same teach-in-English-then-translate shape
// correctionSystemPrompt uses for "explanation"/"explanationTranslation".
// Meant to be called once, when the learner explicitly ends a conversation.
// Chat and Analysis drafts stay internal and only Judge's terminal result is
// returned, the same quality and presentation policy GenerateTitle,
// correct()/compact() use. Callers should
// skip this call entirely when issues is empty (see
// sessionStudySummaryHandler) rather than spend an LLM call being told
// there's nothing to report.
func (p *Pipeline) GenerateStudySummary(ctx context.Context, issues []StudyIssue) ([]protocol.StudySummarySentence, error) {
	raw, err := p.analyze(ctx, studySummarySystemPrompt(p.FeedbackLang), renderStudySummaryInput(issues), true)
	if err != nil {
		return nil, err
	}
	parsed, err := parseJSON[struct {
		Sentences []protocol.StudySummarySentence `json:"sentences"`
	}](raw, "study summary")
	if err != nil {
		return nil, err
	}
	return parsed.Sentences, nil
}

// studySummarySystemPrompt builds GenerateStudySummary's prompt, reusing the
// same native-language config as correctionSystemPrompt/translationSystemPrompt.
// The wrap-up itself is authored in English, sentence by sentence, each
// immediately paired with its native-language translation — same reasoning
// as correctionSystemPrompt's explanation/explanationTranslation split: the
// learner reads real English first, with the translation there only to make
// sure the meaning actually lands.
func studySummarySystemPrompt(lang string) string {
	native := languageName(lang)
	return fmt.Sprintf(`You are an encouraging English-conversation coach wrapping up one practice
session with a %[1]s-speaking learner. You are given every grammar/vocabulary/
phrasing/context issue flagged during the session, each paired with the
learner's original sentence.
Write a short wrap-up that helps the learner study on their own afterward:
- Group individual issues into the recurring PATTERNS behind them (e.g.
  repeated third-person -s omission, article misuse, a specific mistranslated
  collocation) instead of re-listing each correction one by one.
- Call out 2-4 concrete things to focus on next, roughly ordered by how often
  they came up.
- End on an encouraging note.
Write the wrap-up in English, as a sequence of short, complete sentences (a
few short paragraphs or a short list is fine, but split it into individual
sentences rather than one long block) — natural, encouraging coaching
language, not a dry report.
Return STRICT JSON only, no prose, in exactly this shape:
{"sentences":[{"english":"<one sentence of the wrap-up, in English>","translation":"<natural %[1]s translation of that same sentence>"}]}
Rules:
- "english" MUST stay in English.
- "translation" MUST be a translation of "english", not new or different content.
- Every sentence of the wrap-up must appear as its own array entry, in reading order.`, native)
}

func renderStudySummaryInput(issues []StudyIssue) string {
	var b strings.Builder
	b.WriteString("Issues flagged during this session:\n")
	for _, si := range issues {
		fmt.Fprintf(&b, "- sentence: %q | type: %s | %q -> %q | %s\n",
			si.Text, si.Issue.Type, si.Issue.Span, si.Issue.Suggestion, si.Issue.Explanation)
	}
	return b.String()
}

// GenerateStudyQuiz synthesizes a short fill-in-the-blank practice quiz from
// every grammar/vocabulary/phrasing/context issue flagged across a session —
// a way to test whether a learner actually absorbed GenerateStudySummary's
// prose wrap-up, not just read it. Each question blanks out the word/phrase
// one recurring pattern is about, in a fresh example sentence rather than
// the learner's own original wording, so answering it requires applying the
// rule instead of recalling a specific sentence. Same ordered-cascade quality
// bar as GenerateStudySummary — a wrong "correct" answer here would
// actively mislead a learner practicing on their own. Generated on demand,
// only when a learner opens the quiz (see httpserver.sessionQuizHandler),
// not automatically alongside the wrap-up — callers should skip this call
// entirely when issues is empty, same reasoning as GenerateStudySummary.
func (p *Pipeline) GenerateStudyQuiz(ctx context.Context, issues []StudyIssue) ([]protocol.QuizQuestion, error) {
	raw, err := p.analyze(ctx, quizSystemPrompt(p.FeedbackLang), renderStudySummaryInput(issues), true)
	if err != nil {
		return nil, err
	}
	parsed, err := parseJSON[struct {
		Questions []protocol.QuizQuestion `json:"questions"`
	}](raw, "study quiz")
	if err != nil {
		return nil, err
	}
	return parsed.Questions, nil
}

// quizSystemPrompt builds GenerateStudyQuiz's prompt, reusing the same
// native-language config and issue-listing input (renderStudySummaryInput)
// as studySummarySystemPrompt.
func quizSystemPrompt(lang string) string {
	native := languageName(lang)
	return fmt.Sprintf(`You are an English-conversation coach building a short practice quiz from
every grammar/vocabulary/phrasing/context issue flagged during one learner's
practice session, each paired with the learner's original sentence.
Write 3-5 fill-in-the-blank questions that test the recurring PATTERNS behind
those issues (e.g. repeated third-person -s omission, article misuse, a
specific mistranslated collocation) — write a FRESH example sentence for
each pattern rather than reusing the learner's original sentence verbatim,
so answering requires applying the rule, not recalling a specific sentence.
For each question:
- "prompt" is a natural English sentence with exactly one blank, written as
  "___", where the tested word or phrase belongs.
- "answer" is the exact word or phrase that correctly fills that blank,
  including the grammatical form required by the sentence (for example,
  use "reflects" rather than the base form "reflect" in "He ___ on it").
- "answerMeaning" is a SHORT %[1]s gloss of "answer" alone — its meaning as
  used in this sentence, not a translation of the whole sentence. Shown to
  the learner next to "prompt" BEFORE they answer: a blanked English
  sentence alone can fit many different words, so pair the blank with what
  it means so there's something concrete to guess from.
- "acceptableAnswers" lists other words/phrases (an empty array is fine)
  that would ALSO correctly and naturally fill the same blank with
  essentially the same meaning — close synonyms or alternate forms a
  learner could reasonably type instead of "answer". Never include "answer"
  itself, and never include a word that would change the sentence's meaning
  or make it ungrammatical.
- "translation" is a natural %[1]s translation of the FULL sentence with the
  blank correctly filled in, so the learner can check they understood the
  meaning even if they get the blank wrong.
- "explanation" briefly says, in English, why "answer" specifically is the
  best fit for THIS sentence — if "acceptableAnswers" is non-empty, contrast
  "answer" against them (a difference in nuance, formality, or collocation)
  rather than only restating the grammar rule, so the learner comes away
  with a feel for why this exact word fits here, not just that it's
  correct.
- "explanationTranslation" is a natural %[1]s translation of "explanation".
Return STRICT JSON only, no prose, in exactly this shape:
{"questions":[{"prompt":"<sentence with one ___ blank>","answer":"<the word/phrase that fills it>","answerMeaning":"<short %[1]s gloss of answer alone>","acceptableAnswers":["<other word/phrase that also fits>"],"translation":"<%[1]s translation of the full correct sentence>","explanation":"<why this word specifically, in English>","explanationTranslation":"<%[1]s translation of explanation>"}]}
Rules:
- "prompt", "answer", "acceptableAnswers", and "explanation" MUST stay in English.
- "answerMeaning", "translation", and "explanationTranslation" MUST be written in %[1]s.
- Every "prompt" must contain exactly one "___".
- "acceptableAnswers" must never contain "answer" itself, and may be an empty array.
- Prefer variety: don't test the same single pattern more than twice.`, native)
}

// CheckQuizAnswer asks Chat whether a learner's typed quiz answer should count as
// correct when it didn't already match QuizQuestion.Answer or
// AcceptableAnswers verbatim (see QuizPanel's client-side isQuizAnswerAccepted,
// apps/web/src/App.tsx) — a learner may type a genuine synonym the model
// didn't think to list at quiz-generation time. Deliberately biased toward
// "no": httpserver.quizAnswerCheckHandler only calls this after the cheap
// exact-match check already failed, so a false negative here just shows the
// intended answer (mildly annoying, already the pre-existing behavior), while
// a false positive would actively teach the learner something wrong — worse
// than being marked wrong for a right answer. This public method remains the
// immutable, low-latency verdict. quizAnswerCheckHandler additionally passes
// the exact raw Chat draft to RefineQuizAnswerFromDraft in the background and
// caches that terminal result for the learner's next equivalent check.
func (p *Pipeline) CheckQuizAnswer(ctx context.Context, prompt, canonicalAnswer string, acceptableAnswers []string, learnerAnswer string) (bool, error) {
	correct, _, err := p.CheckQuizAnswerFast(ctx, prompt, canonicalAnswer, acceptableAnswers, learnerAnswer)
	return correct, err
}

// CheckQuizAnswerFast returns both the parsed verdict and its raw Chat JSON.
// The raw value is the lossless handoff to RefineQuizAnswerFromDraft; callers
// must not recreate it from the bool or ask Chat a second time.
func (p *Pipeline) CheckQuizAnswerFast(ctx context.Context, prompt, canonicalAnswer string, acceptableAnswers []string, learnerAnswer string) (correct bool, chatDraft string, err error) {
	input := renderQuizAnswerCheckInput(prompt, canonicalAnswer, acceptableAnswers, learnerAnswer)
	raw, err := p.chatDraft(ctx, quizAnswerCheckSystemPrompt, input, true)
	if err != nil {
		return false, "", err
	}
	correct, err = parseQuizAnswerVerdict(raw)
	if err != nil {
		return false, raw, err
	}
	return correct, raw, nil
}

// RefineQuizAnswerFromDraft completes Analysis -> Judge from the exact Chat
// verdict already returned to the learner. The caller stores this result for
// future equivalent checks; it never rewrites the verdict already shown in the
// current quiz attempt.
func (p *Pipeline) RefineQuizAnswerFromDraft(ctx context.Context, prompt, canonicalAnswer string, acceptableAnswers []string, learnerAnswer, chatDraft string) (bool, error) {
	input := renderQuizAnswerCheckInput(prompt, canonicalAnswer, acceptableAnswers, learnerAnswer)
	raw, err := p.analyzeFromDraft(ctx, quizAnswerCheckSystemPrompt, input, true, chatDraft)
	if err != nil {
		return false, err
	}
	return parseQuizAnswerVerdict(raw)
}

func parseQuizAnswerVerdict(raw string) (bool, error) {
	parsed, err := parseJSON[struct {
		Correct bool `json:"correct"`
	}](raw, "quiz answer check")
	if err != nil {
		return false, err
	}
	return parsed.Correct, nil
}

// quizAnswerCheckSystemPrompt asks in English regardless of FeedbackLang: the
// verdict is a plain boolean consumed by QuizPanel, never shown to the
// learner, so there's no native-language output to configure — same
// reasoning as learnerProfileSystemPrompt.
const quizAnswerCheckSystemPrompt = `You are grading one fill-in-the-blank English quiz answer. You will be given
the sentence with its blank, the accepted answer(s) for that blank, and a
learner's typed answer that did not literally match any of them.
Decide whether the learner's answer would ALSO correctly and naturally fill
the blank with essentially the same meaning as the accepted answer(s) — a
genuine synonym or equivalent form, not merely a related or plausible-looking
word.
Default to false unless you are confident the learner's answer is correct: a
wrong "true" verdict teaches the learner something incorrect, which is worse
than a correct answer being marked wrong. Answer false if the learner's
answer would change the sentence's meaning, grammaticality, or register.
Return STRICT JSON only, no prose, in exactly this shape:
{"correct": true or false}`

func renderQuizAnswerCheckInput(prompt, canonicalAnswer string, acceptableAnswers []string, learnerAnswer string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Sentence: %q\n", prompt)
	fmt.Fprintf(&b, "Accepted answer: %q\n", canonicalAnswer)
	if len(acceptableAnswers) > 0 {
		quoted := make([]string, len(acceptableAnswers))
		for i, a := range acceptableAnswers {
			quoted[i] = fmt.Sprintf("%q", a)
		}
		fmt.Fprintf(&b, "Also accepted: %s\n", strings.Join(quoted, ", "))
	}
	fmt.Fprintf(&b, "Learner's answer: %q\n", learnerAnswer)
	return b.String()
}
