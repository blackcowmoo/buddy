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

// SuggestPhraseHints generates the pre-search hints for every key phrase of
// one finished article in a single batched call — PhraseHint's non-revealing
// contract (see phraseHintSystemPrompt), paid once while the article is
// generated (transport.runArticlePhrases) instead of once per learner tap, so
// each chip's feeling is already there the instant the learner taps it. The
// result is keyed by the exact stored phrase strings — the same keys the
// browser matches tapped text against — so persisting and looking up both
// stay exact-match. Entries whose phrase the model mangled beyond a
// case/space difference, or whose hint breaks PhraseHint's validation rules,
// are dropped; the learner's tap then falls back to a single PhraseHint.
func (p *Pipeline) SuggestPhraseHints(ctx context.Context, summary string, phrases []string) (map[string]string, error) {
	if len(phrases) == 0 {
		return nil, fmt.Errorf("phrase hints: no phrases to hint")
	}
	input := fmt.Sprintf("paragraph: %s\nexpressions:\n- %s", summary, strings.Join(phrases, "\n- "))
	return chatJSON(ctx, p, phraseHintsSystemPrompt(p.FeedbackLang), input, func(raw string) (map[string]string, error) {
		return parsePhraseHints(raw, phrases)
	})
}

// parsePhraseHints pairs each returned entry with the requested phrase it
// belongs to. Unknown phrases (a hallucinated expression that isn't in the
// stored list would be a key no reader ever looks up) and hints that break
// the single-hint rules are dropped rather than failing the whole batch —
// partial coverage is still better than none, and the gaps keep the lazy
// tap-time path. Zero surviving hints is an error so the durable job retries
// instead of caching nothing (see newsarticle.Article.KeyPhraseHints: empty
// means "not generated").
func parsePhraseHints(raw string, phrases []string) (map[string]string, error) {
	parsed, err := parseJSON[struct {
		Hints []struct {
			Phrase string `json:"phrase"`
			Hint   string `json:"hint"`
		} `json:"hints"`
	}](raw, "phrase hints")
	if err != nil {
		return nil, err
	}
	wanted := make(map[string]string, len(phrases))
	for _, phrase := range phrases {
		wanted[normalizeHintPhrase(phrase)] = phrase
	}
	out := make(map[string]string, len(phrases))
	for _, item := range parsed.Hints {
		stored, ok := wanted[normalizeHintPhrase(item.Phrase)]
		if !ok {
			continue
		}
		hint := strings.Join(strings.Fields(item.Hint), " ")
		if hint == "" || len([]rune(hint)) > maxPhraseHintRunes {
			continue
		}
		if _, dup := out[stored]; !dup {
			out[stored] = hint
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("phrase hints: no usable hint for any of the %d listed expressions", len(phrases))
	}
	return out, nil
}

// normalizeHintPhrase absorbs the trivial drift (case, padding, repeated
// spaces) between a model echoing a listed phrase back as a JSON key and the
// stored phrase string. It is deliberately weaker than normalizePhraseMatch:
// these keys must stay recognizable as the exact stored strings, so only
// whitespace and case are folded, never punctuation.
func normalizeHintPhrase(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
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
%s`, native, phraseHintRules(native))
}

// phraseHintsSystemPrompt is SuggestPhraseHints's batch wrapper around the
// very rules PhraseHint enforces one-at-a-time: identical hint quality,
// one call for a whole article's phrase list.
func phraseHintsSystemPrompt(lang string) string {
	native := languageName(lang)
	return fmt.Sprintf(`You help a %[1]s-speaking English learner who is about to read one news
study paragraph. For EACH English expression in the input's "expressions"
list, give a HINT that conveys a general sense of the expression without
revealing its meaning, as if the learner had just tapped it.
You will be given "paragraph" (the text the expressions appeared in) and
"expressions" (the exact English expressions to hint on). Point at each idea
from afar: the situation, imagery, topic, or feeling the expression evokes,
so the learner can roughly guess what it is about — and still has to search
to learn its actual meaning.
Return STRICT JSON only, no prose, in exactly this shape:
{"hints":[{"phrase":"<expression 1>","hint":"<one short %[1]s sentence>"},{"phrase":"<expression 2>","hint":"..."}]}
Copy every "phrase" value EXACTLY as listed — never reword, re-order, or
normalize it — and give every listed expression exactly one entry.
%s`, native, phraseHintRules(native))
}

// phraseHintRules is the shared no-answer contract both the single and batch
// hint prompts send, so the two paths can never drift apart on what makes a
// hint a hint (assertPhraseHintNonRevealingPrompt pins it).
func phraseHintRules(native string) string {
	return fmt.Sprintf(`Rules:
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
