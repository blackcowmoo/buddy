package pipeline

import (
	"context"
	"fmt"
	"math/rand"
	"strings"

	"buddy/server/internal/protocol"
)

// articleQuizChoiceCount is how many candidate translations
// GenerateArticleStudy asks for — matches the classic 4-option
// multiple-choice shape WordReview's recognition-mode quiz already uses.
const articleQuizChoiceCount = 4

// GenerateArticleStudy turns one newsfeed.Candidate's headline/snippet into
// a single self-contained English study paragraph, plus a native-language
// multiple-choice quiz that tests whether the learner actually understood
// it — see protocol.ArticleStudy's doc for the exact shape. Deliberately
// built from the feed's own short editorial snippet, not a scrape of the
// full article body: full-page scraping is fragile per outlet (paywalls,
// changing markup, anti-bot measures), and a snippet is already enough
// material for a coherent study paragraph. Generated once per unique
// article URL and cached (see newsarticle.Store.SaveArticle) — every later
// learner who draws the same story reads this exact result, never re-paying
// the LLM call. Same Analysis-ensemble+Judge quality bar as
// GenerateStudyQuiz: a wrong "correct" choice here would actively mislead a
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
	// here, once, before this result is ever cached: every later learner who
	// draws the same story (see newsarticle.Store.SaveArticle) sees the same
	// already-shuffled order.
	return shuffleChoices(study), nil
}

// shuffleChoices randomizes s.Choices' order and remaps CorrectIndex to
// follow the same choice — see GenerateArticleStudy's call site for why.
func shuffleChoices(s protocol.ArticleStudy) protocol.ArticleStudy {
	order := rand.Perm(len(s.Choices))
	shuffled := make([]string, len(s.Choices))
	newCorrectIndex := 0
	for newPos, oldPos := range order {
		shuffled[newPos] = s.Choices[oldPos]
		if oldPos == s.CorrectIndex {
			newCorrectIndex = newPos
		}
	}
	s.Choices = shuffled
	s.CorrectIndex = newCorrectIndex
	return s
}

// validateArticleStudy guards the invariants httpserver/newsarticle trust
// without re-checking: exactly one correct choice, in range, and every
// field actually populated. An LLM occasionally drifts from the requested
// JSON shape (e.g. 3 choices instead of 4) even under strict JSON mode, and
// a malformed quiz stored as-is would surface as an out-of-range index or a
// broken multiple-choice question days later, whenever it's drawn — better
// to fail the draw immediately so the caller can retry with a different
// candidate article instead.
func validateArticleStudy(s protocol.ArticleStudy) error {
	if strings.TrimSpace(s.Summary) == "" {
		return fmt.Errorf("article study: empty summary")
	}
	if len(s.Choices) != articleQuizChoiceCount {
		return fmt.Errorf("article study: got %d choices, want %d", len(s.Choices), articleQuizChoiceCount)
	}
	if s.CorrectIndex < 0 || s.CorrectIndex >= len(s.Choices) {
		return fmt.Errorf("article study: correctIndex %d out of range", s.CorrectIndex)
	}
	if strings.TrimSpace(s.Explanation) == "" {
		return fmt.Errorf("article study: empty explanation")
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
1. Write ONE short, self-contained English paragraph (3-5 sentences)
   summarizing the story, suitable for an intermediate English learner:
   clear, natural sentences, no jargon left unexplained. Base it only on the
   headline and snippet given — do not invent specific facts, quotes, or
   numbers beyond what they state or directly imply.
2. Write a %[1]s-language reading-comprehension check for that exact
   paragraph: exactly %[2]d candidate %[1]s translations/interpretations of
   the paragraph's meaning, with exactly ONE of them accurate. The other
   %[3]d must be PLAUSIBLE, not obviously wrong — each should read like a
   reasonable translation at a glance, but subtly misrepresent the
   paragraph in one concrete way (e.g. swap which party did what, flip a
   negation, change a number/date, swap a similar-sounding entity) rather
   than being unrelated or nonsensical. A learner who has not actually read
   and understood the English paragraph should find this genuinely hard to
   guess.
Return STRICT JSON only, no prose, in exactly this shape:
{"summary":"<the English paragraph>","choices":["<%[1]s option 1>","<%[1]s option 2>","<%[1]s option 3>","<%[1]s option 4>"],"correctIndex":<0-based index of the one accurate option>,"explanation":"<%[1]s explanation of why that option is accurate, contrasting it against what the wrong options got wrong>"}
Rules:
- "summary" MUST stay in English.
- "choices" and "explanation" MUST be written in %[1]s.
- "choices" MUST contain exactly %[2]d entries.
- Exactly one entry in "choices" may be an accurate translation of "summary" — the rest must each contain a real, concrete inaccuracy.`, native, articleQuizChoiceCount, articleQuizChoiceCount-1)
}

func renderArticleStudyInput(source, title, description string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Source: %s\n", source)
	fmt.Fprintf(&b, "Headline: %s\n", title)
	fmt.Fprintf(&b, "Snippet: %s\n", description)
	return b.String()
}
