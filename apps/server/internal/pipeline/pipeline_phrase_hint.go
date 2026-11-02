package pipeline

import (
	"context"
	"fmt"
	"strings"
)

// maxPhraseHintRunes caps how long one hint may be. A hint is a glanceable
// nudge beside the tapped phrase; a reply longer than this is the model
// slipping into a full explanation, so it is rejected (the durable job
// retries) rather than truncated into a half-answer.
const maxPhraseHintRunes = 160

// PhraseHint generates the pre-search meaning hint for one tapped article
// phrase: a short native-language nudge that conveys the general sense of an
// idiom or set phrase WITHOUT revealing the dictionary gloss the learner
// would memorize. The actual search keeps using DefineWord and its
// answer contract unchanged; hint and definition are cached independently
// (see wordlookup.HintKey), so seeing one never affects the other.
func (p *Pipeline) PhraseHint(ctx context.Context, phrase, passage string) (string, error) {
	decode := func(raw string) (string, error) { return parsePhraseHint(raw) }
	return chatJSON(ctx, p, phraseHintSystemPrompt(p.FeedbackLang), fmt.Sprintf("phrase: %s\ncontext: %s", phrase, passage), decode)
}

func parsePhraseHint(raw string) (string, error) {
	parsed, err := parseJSON[struct {
		Hint string `json:"hint"`
	}](raw, "phrase hint")
	if err != nil {
		return "", err
	}
	hint := strings.TrimSpace(parsed.Hint)
	if hint == "" {
		return "", fmt.Errorf("phrase hint: model returned an empty hint")
	}
	if len([]rune(hint)) > maxPhraseHintRunes {
		return "", fmt.Errorf("phrase hint: hint exceeds %d characters", maxPhraseHintRunes)
	}
	return hint, nil
}

// phraseHintSystemPrompt builds PhraseHint's prompt. Unlike every vocabulary
// prompt in this package it deliberately stops short of the memorizable
// answer (see dictionaryMeaningRules): the learner taps a phrase to get a
// feel for it, and still has to run a real search for the definition.
func phraseHintSystemPrompt(lang string) string {
	native := languageName(lang)
	return fmt.Sprintf(`You help a %[1]s-speaking English learner who tapped an English idiom or
set phrase while reading, before they look it up. Give a HINT that conveys a
general sense of the expression without revealing its meaning.
You will be given "phrase" (the exact English expression they tapped) and
"context" (the passage it appeared in). Point at the idea from afar: the
situation, imagery, topic, or feeling the expression evokes, so the learner
can roughly guess what it is about — and still has to search to learn its
actual meaning.
Return STRICT JSON only, no prose, in exactly this shape:
{"hint":"<one short %[1]s sentence>"}
Rules:
- "hint" MUST be one short %[1]s sentence.
- Convey only a general sense: point at the situation, imagery, topic, or
  feeling around the meaning instead of stating the meaning.
- "hint" MUST NOT give or closely paraphrase the expression's translation or
  dictionary gloss, and MUST NOT contain wording the learner could memorize
  as the answer. If the hint alone would answer a "what does this mean?"
  quiz, it went too far.
- When the expression's literal imagery differs from its actual meaning,
  using that imagery as the clue is good — it is hinting, not answering.
Korean hint examples (follow the requested JSON wrapper; the memorized answer
is shown only to illustrate what MUST NOT appear):
Context: "The host told a joke to break the ice.", answer: 어색한 분위기를 풀다
Good: {"hint":"서먹서먹한 첫 만남을 떠올리게 하는 행동과 관련돼요."}
Bad: {"hint":"어색한 분위기를 풀어 준다는 뜻이에요."} — that is the answer itself.
Context: "The company cut corners on materials.", answer: 비용을 줄이려고 질을 타협하다
Good: {"hint":"무언가의 '모서리'를 대충 다룬다는 모양새에서 나온 표현이에요."}
Bad: {"hint":"비용을 아끼려고 질을 낮추는 행위."} — that is the answer itself.
Do not copy these examples' words when the input refers to a different
expression.`, native)
}
