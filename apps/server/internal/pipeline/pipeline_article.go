package pipeline

import (
	"context"
	"fmt"
	"math/rand"
	"strings"

	"buddy/server/internal/protocol"
)

// articleQuizMinSubQuestions is the floor GenerateArticleStudy enforces —
// not a fixed count: the LLM decides how many independent, checkable facts
// the summary actually supports (see articleStudySystemPrompt), but fewer
// than 2 wouldn't be a meaningful comprehension check at all.
const articleQuizMinSubQuestions = 2

// GenerateArticleStudy turns one newsfeed.Candidate's headline/snippet into
// a single self-contained English study paragraph, plus a native-language
// quiz — several independent 2-choice sub-questions, each isolating one
// concrete fact from the paragraph (see protocol.ArticleStudy's doc for why
// this shape, not one 4-choice "which paraphrase is right" question).
// Deliberately built from the feed's own short editorial snippet, not a
// scrape of the full article body: full-page scraping is fragile per outlet
// (paywalls, changing markup, anti-bot measures), and a snippet is already
// enough material for a coherent study paragraph. Generated once per unique
// article URL and cached (see newsarticle.Store.SaveArticle) — every later
// learner who draws the same story reads this exact result, never re-paying
// the LLM call. Same Analysis-ensemble+Judge quality bar as
// GenerateStudyQuiz: a wrong "correct" option here would actively mislead a
// learner practicing on their own, so this affords the full ensemble rather
// than a single quick call.
func (p *Pipeline) GenerateArticleStudy(ctx context.Context, source, title, description string) (protocol.ArticleStudy, error) {
	raw, err := p.analyze(ctx, articleStudySystemPrompt(p.FeedbackLang), renderArticleStudyInput(source, title, description), true)
	if err != nil {
		return protocol.ArticleStudy{}, err
	}
	study, err := parseJSON[protocol.ArticleStudy](raw, "article study")
	if err != nil {
		return protocol.ArticleStudy{}, err
	}
	if err := validateArticleStudy(study); err != nil {
		return protocol.ArticleStudy{}, err
	}
	// An LLM asked for "the accurate option" tends to place it at the same
	// position (often index 0) far more often than chance — a learner could
	// learn that pattern instead of actually reading the summary. Reshuffle
	// here, once per sub-question, before this result is ever cached: every
	// later learner who draws the same story (see newsarticle.Store.
	// SaveArticle) sees the same already-shuffled order.
	return shuffleSubQuestions(study), nil
}

// shuffleSubQuestions independently coin-flips each sub-question's two
// options — see GenerateArticleStudy's call site for why.
func shuffleSubQuestions(s protocol.ArticleStudy) protocol.ArticleStudy {
	for i, q := range s.SubQuestions {
		if rand.Intn(2) == 0 {
			continue
		}
		q.Options[0], q.Options[1] = q.Options[1], q.Options[0]
		q.CorrectOptionIndex = 1 - q.CorrectOptionIndex
		s.SubQuestions[i] = q
	}
	return s
}

// validateArticleStudy guards the invariants httpserver/newsarticle trust
// without re-checking: every sub-question has exactly 2 options and an
// in-range correct index, and every field is actually populated. An LLM
// occasionally drifts from the requested JSON shape even under strict JSON
// mode, and a malformed quiz stored as-is would surface as a broken
// question days later, whenever it's drawn — better to fail the draw
// immediately so the caller can retry with a different candidate article
// instead.
func validateArticleStudy(s protocol.ArticleStudy) error {
	if strings.TrimSpace(s.Summary) == "" {
		return fmt.Errorf("article study: empty summary")
	}
	if strings.TrimSpace(s.Translation) == "" {
		return fmt.Errorf("article study: empty translation")
	}
	if len(s.SubQuestions) < articleQuizMinSubQuestions {
		return fmt.Errorf("article study: got %d sub-questions, want at least %d", len(s.SubQuestions), articleQuizMinSubQuestions)
	}
	for i, q := range s.SubQuestions {
		if strings.TrimSpace(q.Prompt) == "" {
			return fmt.Errorf("article study: sub-question %d: empty prompt", i)
		}
		if len(q.Options) != 2 {
			return fmt.Errorf("article study: sub-question %d: got %d options, want 2", i, len(q.Options))
		}
		for j, opt := range q.Options {
			if strings.TrimSpace(opt) == "" {
				return fmt.Errorf("article study: sub-question %d: empty option %d", i, j)
			}
		}
		if q.CorrectOptionIndex != 0 && q.CorrectOptionIndex != 1 {
			return fmt.Errorf("article study: sub-question %d: correctOptionIndex %d out of range", i, q.CorrectOptionIndex)
		}
		if strings.TrimSpace(q.Explanation) == "" {
			return fmt.Errorf("article study: sub-question %d: empty explanation", i)
		}
	}
	return nil
}

func articleStudySystemPrompt(lang string) string {
	native := languageName(lang)
	return fmt.Sprintf(`You are an English-conversation coach picking one short news story for a
%[1]s-speaking learner to practice reading comprehension on. You are given a
news article's source outlet, headline, and a short editorial snippet (not
the full article body).
Do two things:
1. Write ONE short, self-contained English paragraph summarizing the story,
   suitable for an intermediate English learner: clear, natural sentences,
   no jargon left unexplained. A bit longer than a bare minimum summary is
   fine — enough sentences to carry several distinct, independently
   checkable facts (a specific amount, date, name, or who-did-what), since
   the quiz below needs that many separate facts to draw on. Base it only
   on the headline and snippet given — do not invent specific facts,
   quotes, or numbers beyond what they state or directly imply.
2. Write a %[1]s-language reading-comprehension check for that exact
   paragraph, as a series of INDEPENDENT sub-questions — NOT one question
   with several candidate paraphrases of the whole paragraph. Each
   sub-question must:
   - Target exactly ONE concrete, independently-checkable fact from the
     paragraph (an amount, a date, a name, which party did what, a
     number) — never the paragraph's overall meaning.
   - Have a short %[1]s "prompt" naming which fact it's asking about (e.g.
     "인수 금액은 얼마인가요?"), not a full-sentence restatement.
   - Have exactly 2 short %[1]s "options" answering that prompt — not full
     paraphrases of the paragraph — one of them (at "correctOptionIndex")
     matching the paragraph, the other a plausible near-miss that changes
     THAT ONE fact only (a different number, currency, date, or the
     opposite of what happened) while staying otherwise identical in
     wording, so it can't be told apart from the correct one just by
     glancing at how it's phrased.
   - Come with its own %[1]s "explanation" of why its correct option
     matches the paragraph.
   Write as many sub-questions as the paragraph genuinely supports with
   distinct, independently-checkable facts — at least %[2]d, more if the
   paragraph has more distinct facts worth checking. Every sub-question
   must test a DIFFERENT fact from every other one: two sub-questions
   about the same fact (even phrased differently) are not acceptable. A
   learner who has not actually read and understood the English paragraph
   should find each sub-question genuinely hard to guess on its own,
   independent of the others.
Return STRICT JSON only, no prose, in exactly this shape:
{"summary":"<the English paragraph>","translation":"<the full natural translation of the English paragraph>","subQuestions":[{"prompt":"<%[1]s question about one fact>","options":["<%[1]s option 1>","<%[1]s option 2>"],"correctOptionIndex":<0 or 1>,"explanation":"<%[1]s explanation>"}, ...]}
Rules:
- "summary" MUST stay in English.
- "translation" MUST be a complete, natural %[1]s translation of the entire "summary", preserving every fact.
- "prompt", "options", and "explanation" MUST be written in %[1]s.
- Each "options" array MUST contain exactly 2 entries.
- At least %[2]d entries in "subQuestions", each about a different fact from the paragraph.`, native, articleQuizMinSubQuestions)
}

func renderArticleStudyInput(source, title, description string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Source: %s\n", source)
	fmt.Fprintf(&b, "Headline: %s\n", title)
	fmt.Fprintf(&b, "Snippet: %s\n", description)
	return b.String()
}
