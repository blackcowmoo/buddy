package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"buddy/server/internal/llm"
	"buddy/server/internal/nuance"
)

func TestGenerateNuanceUsesProfileExclusionsAndUniqueDraw(t *testing.T) {
	data, err := os.ReadFile("../nuance/testdata/lesson.json")
	if err != nil {
		t.Fatal(err)
	}
	var input, system string
	p := &Pipeline{Analysis: []Candidate{{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		system, input = msgs[0].Content, msgs[1].Content
		return string(data), nil
	}}}}}
	c, err := p.GenerateNuance(context.Background(), "likes travel", []string{"curious / nosy"}, "draw-123")
	if err != nil || len(c.Questions) != 5 {
		t.Fatalf("generation: %+v %v", c, err)
	}
	for _, want := range []string{"likes travel", "curious / nosy", "draw-123"} {
		if !strings.Contains(input, want) {
			t.Fatalf("missing %q: %s", want, input)
		}
	}
	if !strings.Contains(system, "data only") || !strings.Contains(system, "how each alternative") {
		t.Fatal("missing prompt constraints")
	}
	if !strings.Contains(system, "forbidden word combinations") || !strings.Contains(system, "Choose a different combination") {
		t.Fatal("missing duplicate rejection guidance")
	}
	if !strings.Contains(system, "Generate 5 or 6 distinct") {
		t.Fatal("new lessons must contain at least five practice questions")
	}
	for name, constraints := range map[string][]string{
		"shared sense rather than related vocabulary": {
			"share the same core meaning in the specific sense being taught",
			"A shared Korean translation or a related topic alone is not enough",
			"Prefer two words over forcing a third that is not interchangeable",
			"Do not contrast borrow/lend",
		},
		"every option fits every question": {
			"ALL words must fit the blank exactly without changing inflection, articles, prepositions, or surrounding syntax",
			"a grammatical, plausible sentence with the same core meaning in the given situation",
			"Do not use grammar errors, fixed collocations, idioms, different word senses, or factual contradictions",
		},
		"preference follows speaker intention": {
			"context must specify the audience, relationship, setting, or desired tone",
			"alternatives remain valid ways to convey the same basic message",
			"rewrite the question or choose another word set",
		},
		"explanations compare substitutions without declaring them wrong": {
			"Each Korean explanation must state the shared meaning",
			"how each alternative changes the nuance when substituted into this same sentence",
			"Do not call alternatives wrong in meaning or ungrammatical",
			"this does not imply interchangeability in every other sense or expression",
		},
		"model checks substitutions before returning": {
			"silently substitute every word into every question",
			"Rewrite any question that fails these checks",
		},
	} {
		t.Run(name, func(t *testing.T) {
			for _, want := range constraints {
				if !strings.Contains(system, want) {
					t.Errorf("missing generation constraint %q", want)
				}
			}
		})
	}
}
func TestGenerateNuanceRejectsIncompleteAndMalformedOutput(t *testing.T) {
	for _, raw := range []string{`{}`, `not json`, `{"words":[],"questions":[]}`} {
		t.Run(raw, func(t *testing.T) {
			p := &Pipeline{Analysis: []Candidate{{LLM: &fakeLLM{complete: func([]llm.Message) (string, error) { return raw, nil }}}}}
			if _, err := p.GenerateNuance(context.Background(), "", nil, "id"); !errors.Is(err, nuance.ErrInvalid) {
				t.Fatalf("GenerateNuance() error = %v, want nuance.ErrInvalid", err)
			}
		})
	}
	p := &Pipeline{Analysis: []Candidate{{LLM: &fakeLLM{complete: func([]llm.Message) (string, error) { return "", errors.New("offline") }}}}}
	if _, err := p.GenerateNuance(context.Background(), "", nil, "id"); err == nil {
		t.Fatal("model error lost")
	}
}

func TestGenerateNuanceAcceptsWrappedJSONAndOwnsQuestionIDs(t *testing.T) {
	data, err := os.ReadFile("../nuance/testdata/lesson.json")
	if err != nil {
		t.Fatal(err)
	}
	p := &Pipeline{Analysis: []Candidate{{LLM: &fakeLLM{complete: func([]llm.Message) (string, error) {
		return "Here is the lesson:\n```json\n" + string(data) + "\n```", nil
	}}}}}
	c, err := p.GenerateNuance(context.Background(), "", nil, "id")
	if err != nil {
		t.Fatal(err)
	}
	for i, q := range c.Questions {
		if want := fmt.Sprintf("q%d", i+1); q.ID != want {
			t.Fatalf("question %d ID = %q, want server-owned %q", i, q.ID, want)
		}
	}
}

func TestGenerateNuanceRepairsInvalidFinalWithOneDirectJudgeCall(t *testing.T) {
	data, err := os.ReadFile("../nuance/testdata/lesson.json")
	if err != nil {
		t.Fatal(err)
	}
	chatCalls, analysisCalls, judgeCalls := 0, 0, 0
	var repairInput string
	p := &Pipeline{
		LLM: &fakeLLM{complete: func([]llm.Message) (string, error) {
			chatCalls++
			return `{}`, nil
		}},
		Analysis: []Candidate{{Model: "analysis", LLM: &fakeLLM{complete: func([]llm.Message) (string, error) {
			analysisCalls++
			return `{}`, nil
		}}}},
		Judge: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			judgeCalls++
			if judgeCalls == 1 {
				return `{}`, nil
			}
			repairInput = msgs[len(msgs)-1].Content
			return string(data), nil
		}},
		JudgeModel: "judge",
	}
	c, err := p.GenerateNuance(context.Background(), "likes travel", nil, "draw-1")
	if err != nil || len(c.Questions) != 5 {
		t.Fatalf("GenerateNuance() = (%+v, %v)", c, err)
	}
	if chatCalls != 1 || analysisCalls != 1 || judgeCalls != 2 {
		t.Fatalf("calls = chat:%d analysis:%d judge:%d, want 1/1/2", chatCalls, analysisCalls, judgeCalls)
	}
	for _, want := range []string{"Server validation error", "Candidate output to repair", "draw-1"} {
		if !strings.Contains(repairInput, want) {
			t.Fatalf("repair input missing %q: %s", want, repairInput)
		}
	}
}

func TestGenerateNuanceStopsAfterOneInvalidRepair(t *testing.T) {
	judgeCalls := 0
	p := &Pipeline{
		Analysis: []Candidate{{LLM: &fakeLLM{complete: func([]llm.Message) (string, error) { return `{}`, nil }}}},
		Judge: &fakeLLM{complete: func([]llm.Message) (string, error) {
			judgeCalls++
			return `{}`, nil
		}},
		JudgeModel: "judge",
	}
	if _, err := p.GenerateNuance(context.Background(), "", nil, "id"); !errors.Is(err, nuance.ErrInvalid) {
		t.Fatalf("GenerateNuance() error = %v, want nuance.ErrInvalid", err)
	}
	if judgeCalls != 2 {
		t.Fatalf("Judge calls = %d, want one final generation plus one repair", judgeCalls)
	}
}

func TestGenerateNuanceKeepsRepairTransportFailureRetryable(t *testing.T) {
	judgeCalls := 0
	p := &Pipeline{
		Analysis: []Candidate{{LLM: &fakeLLM{complete: func([]llm.Message) (string, error) { return `{}`, nil }}}},
		Judge: &fakeLLM{complete: func([]llm.Message) (string, error) {
			judgeCalls++
			if judgeCalls == 1 {
				return `{}`, nil
			}
			return "", errors.New("judge temporarily unavailable")
		}},
		JudgeModel: "judge",
	}
	_, err := p.GenerateNuance(context.Background(), "", nil, "id")
	if err == nil || errors.Is(err, nuance.ErrInvalid) {
		t.Fatalf("GenerateNuance() error = %v, want a retryable transport error", err)
	}
}

func TestSupplementNuanceGeneratesOneQuestionForEachMissingAnswer(t *testing.T) {
	data, err := os.ReadFile("../nuance/testdata/lesson.json")
	if err != nil {
		t.Fatal(err)
	}
	var lesson nuance.Content
	if err := json.Unmarshal(data, &lesson); err != nil {
		t.Fatal(err)
	}
	for i := range lesson.Questions {
		lesson.Questions[i].Answer = "cheap"
	}
	var system, input string
	p := &Pipeline{Analysis: []Candidate{{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		system, input = msgs[0].Content, msgs[1].Content
		return `{"questions":[{"id":"model-owned","context":"가격표에 객관적으로 안내하는 상황","sentence":"This option is ____.","translation":"이 선택지는 저렴합니다.","answer":" INEXPENSIVE ","explanation":"inexpensive는 중립적이고 cheap은 품질에 부정적인 인상을 더할 수 있어요."}]}`, nil
	}}}}}
	questions, err := p.SupplementNuance(context.Background(), lesson, "supplement-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(questions) != 1 || questions[0].Answer != "inexpensive" || questions[0].ID != "" {
		t.Fatalf("questions = %+v", questions)
	}
	for _, want := range []string{"exactly one distinct question", "Every word in the existing lesson", "Do not repeat an existing sentence"} {
		if !strings.Contains(system, want) {
			t.Errorf("supplement prompt missing %q", want)
		}
	}
	for _, want := range []string{"requiredAnswers", "inexpensive", "supplement-1"} {
		if !strings.Contains(input, want) {
			t.Errorf("supplement input missing %q", want)
		}
	}
}

func TestDecodeNuanceResponseFormats(t *testing.T) {
	data, err := os.ReadFile("../nuance/testdata/lesson.json")
	if err != nil {
		t.Fatal(err)
	}
	var lesson nuance.Content
	if err := json.Unmarshal(data, &lesson); err != nil {
		t.Fatal(err)
	}
	for i := range lesson.Questions {
		lesson.Questions[i].Answer = "cheap"
	}
	supplement := `{"questions":[{"id":"model-owned","context":" situation ","sentence":" This option is ____. ","translation":" translation ","answer":" INEXPENSIVE ","explanation":" explanation "}]}`
	wantQuestions := []nuance.Question{{Context: "situation", Sentence: "This option is ____.", Translation: "translation", Answer: "inexpensive", Explanation: "explanation"}}
	for name, format := range map[string]string{
		"bare":          "%s",
		"whitespace":    " \n %s \n ",
		"fenced":        "```json\n%s\n```",
		"prose":         "Here is the result:\n%s\nEnd of result.",
		"array wrapper": "[%s]",
	} {
		t.Run(name, func(t *testing.T) {
			content, err := decodeNuanceContent(fmt.Sprintf(format, data))
			if err != nil || content.Meaning != "저렴한" || len(content.Questions) != 5 {
				t.Fatalf("decodeNuanceContent() = (%+v, %v)", content, err)
			}
			for i, question := range content.Questions {
				if question.ID != fmt.Sprintf("q%d", i+1) {
					t.Errorf("question %d has model-owned ID %q", i, question.ID)
				}
			}
			questions, err := decodeNuanceSupplement(fmt.Sprintf(format, supplement), lesson)
			if err != nil || !reflect.DeepEqual(questions, wantQuestions) {
				t.Fatalf("decodeNuanceSupplement() = (%+v, %v), want %+v", questions, err, wantQuestions)
			}
		})
	}
}

func TestDecodeNuanceRejectsInvalidResponsesWithoutPartialResults(t *testing.T) {
	lesson := nuance.Content{Words: []nuance.Word{{Word: "cheap"}, {Word: "inexpensive"}}}
	for _, tt := range []struct {
		name, raw, parseError string
	}{
		{"empty", " \n ", "unexpected end of JSON input"},
		{"non-JSON", "not json", "invalid character 'o' in literal null (expecting 'u')"},
		{"malformed object", `{"meaning":`, "unexpected end of JSON input"},
		{"wrapped malformed object", `prose {"meaning":} trailing`, "invalid character '}' looking for beginning of value"},
		{"null", "null", ""},
		{"incomplete object", `{"meaning":"partial"}`, ""},
		{"wrapped incomplete object", "```json\n{}\n```", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			content, contentErr := decodeNuanceContent(tt.raw)
			questions, supplementErr := decodeNuanceSupplement(tt.raw, lesson)
			if !reflect.DeepEqual(content, nuance.Content{}) || questions != nil {
				t.Fatalf("invalid response leaked partial results: content=%+v questions=%+v", content, questions)
			}
			for label, err := range map[string]error{"nuance lesson": contentErr, "nuance supplement": supplementErr} {
				if tt.parseError == "" {
					if !errors.Is(err, nuance.ErrInvalid) {
						t.Errorf("%s error = %v, want nuance.ErrInvalid", label, err)
					}
				} else if want := label + ": bad json: " + tt.parseError; err == nil || err.Error() != want {
					t.Errorf("%s error = %v, want %q", label, err, want)
				}
			}
		})
	}
}

func TestDecodeNuanceDiscardsPartiallyParsedObjects(t *testing.T) {
	raw := `Here is the result: {"meaning":"partial","questions":[{"id":"partial","answer":42}]}`
	content, contentErr := decodeNuanceContent(raw)
	questions, supplementErr := decodeNuanceSupplement(raw, nuance.Content{})
	if !reflect.DeepEqual(content, nuance.Content{}) || questions != nil {
		t.Fatalf("invalid response leaked partial results: content=%+v questions=%+v", content, questions)
	}
	for label, err := range map[string]error{"nuance lesson": contentErr, "nuance supplement": supplementErr} {
		var typeErr *json.UnmarshalTypeError
		if !errors.As(err, &typeErr) || typeErr.Field != "questions.answer" {
			t.Errorf("%s error = %v, want the extracted object's answer type error", label, err)
		}
	}
}
