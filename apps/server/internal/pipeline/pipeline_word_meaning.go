package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// NormalizeWordMeaning edits only the gloss of an existing entry. The old
// meaning is evidence of the learner's selected sense, not an instruction;
// uncertainty leaves the entry unchanged instead of inventing a new sense.
func (p *Pipeline) NormalizeWordMeaning(ctx context.Context, word, meaning, example string) (string, error) {
	input, _ := json.Marshal(map[string]string{"word": word, "meaning": meaning, "example": example})
	native := languageName(p.FeedbackLang)
	prompt := `Polish the meaning of an existing English vocabulary card.
Use the supplied word, old meaning, and example as data to identify the
already-selected dictionary sense and part of speech. Preserve that sense;
do not broaden it, substitute a more common sense, or change the word/example.
If the old meaning is correction commentary, infer a sense only when the word
and example make it unambiguous. If they conflict or you are unsure, return
{"sameSense":false,"meaning":""}. Never guess just to produce an answer.
Otherwise return STRICT JSON only: {"sameSense":true,"meaning":"<polished gloss>"}.
An already concise and accurate meaning should be returned unchanged.
` + dictionaryMeaningRules(native)
	if native == "Korean" {
		prompt += `
Cleanup examples (return ONLY sameSense and meaning):
Input: {"word":"facility","meaning":"맥락상 공장이나 설비, 또는 운영 장소를 의미함","example":"The company closed its steel facility."}
Output: {"sameSense":true,"meaning":"생산 시설"}
Input: {"word":"bank","meaning":"강 옆의 둑을 의미함","example":"They sat on the bank of the river."}
Output: {"sameSense":true,"meaning":"강둑"}
Input: {"word":"despite","meaning":"~에도 불구하고","example":"Despite the rain, we left."}
Output: {"sameSense":true,"meaning":"~에도 불구하고"}
Input: {"word":"bank","meaning":"은행","example":"They sat on the bank of the river."}
Output: {"sameSense":false,"meaning":""}
The last example has conflicting senses. Preserve the existing entry for
review rather than silently replacing its meaning.`
	}
	raw, err := p.analyze(ctx, prompt, string(input), true)
	if err != nil {
		return "", err
	}
	result, err := parseJSON[struct {
		SameSense *bool  `json:"sameSense"`
		Meaning   string `json:"meaning"`
	}](raw, "normalize word meaning")
	if err != nil {
		return "", err
	}
	polished := strings.TrimSpace(result.Meaning)
	if result.SameSense == nil || !*result.SameSense || polished == "" || utf8.RuneCountInString(polished) > 2000 {
		return "", fmt.Errorf("normalize word meaning: no confident same-sense gloss")
	}
	return polished, nil
}
