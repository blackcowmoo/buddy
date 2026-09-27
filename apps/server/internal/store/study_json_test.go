package store

import (
	"reflect"
	"testing"

	"buddy/server/internal/protocol"
)

func TestStudyJSONDiscardsLegacyAndMalformedResults(t *testing.T) {
	for _, raw := range []string{
		"", "legacy plain text", "null", "{}",
		`[{"english":"valid first sentence","prompt":"valid first question"},42]`,
		`[{"english":"valid first sentence","prompt":"valid first question"}] trailing`,
	} {
		t.Run(raw, func(t *testing.T) {
			if got := decodeStudySummary(raw); got != nil {
				t.Errorf("summary = %#v, want nil", got)
			}
			if got := decodeQuiz(raw); got != nil {
				t.Errorf("quiz = %#v, want nil", got)
			}
		})
	}
	if got := decodeStudySummary("[]"); got == nil || len(got) != 0 {
		t.Errorf("empty summary array = %#v, want non-nil empty slice", got)
	}
	if got := decodeQuiz("[]"); got == nil || len(got) != 0 {
		t.Errorf("empty quiz array = %#v, want non-nil empty slice", got)
	}
}

func TestStudyJSONRoundTripsBilingualResults(t *testing.T) {
	summary := []protocol.StudySummarySentence{{English: "Use the past tense.", Translation: "과거형을 사용하세요."}}
	quiz := []protocol.QuizQuestion{{Prompt: "I ___ home.", Answer: "went", AcceptableAnswers: []string{"walked"}, Translation: "나는 집에 갔다.", Explanation: "Use the past tense.", ExplanationTranslation: "과거형을 사용하세요."}}
	summaryJSON, err := encodeJSONSlice(summary)
	if err != nil {
		t.Fatal(err)
	}
	quizJSON, err := encodeJSONSlice(quiz)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeStudySummary(summaryJSON); !reflect.DeepEqual(got, summary) {
		t.Errorf("summary = %#v, want %#v", got, summary)
	}
	if got := decodeQuiz(quizJSON); !reflect.DeepEqual(got, quiz) {
		t.Errorf("quiz = %#v, want %#v", got, quiz)
	}
}
