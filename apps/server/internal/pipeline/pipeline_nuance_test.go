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
		"context alone must indicate the intended answer": {
			"State those cues explicitly inside the context text",
			"a reader who knows every word must reach the intended answer from the context alone",
			"Two or more answers feeling equally natural means the context is incomplete",
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
			switch judgeCalls {
			case 1:
				return `{}`, nil
			case 2:
				repairInput = msgs[len(msgs)-1].Content
				return string(data), nil
			default:
				return `{"issues":[]}`, nil
			}
		}},
		JudgeModel: "judge",
	}
	c, err := p.GenerateNuance(context.Background(), "likes travel", nil, "draw-1")
	if err != nil || len(c.Questions) != 5 {
		t.Fatalf("GenerateNuance() = (%+v, %v)", c, err)
	}
	// One cascade final, one structural repair, and one unambiguous clarity
	// audit: a clean audit must never trigger another repair.
	if chatCalls != 1 || analysisCalls != 1 || judgeCalls != 3 {
		t.Fatalf("calls = chat:%d analysis:%d judge:%d, want 1/1/3", chatCalls, analysisCalls, judgeCalls)
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
	for _, want := range []string{"exactly one distinct question", "Every word in the existing lesson", "Do not repeat an existing sentence", "State the cue explicitly inside the context text"} {
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

func TestGenerateNuanceAuditsAndRepairsAmbiguousQuestions(t *testing.T) {
	data, err := os.ReadFile("../nuance/testdata/lesson.json")
	if err != nil {
		t.Fatal(err)
	}
	repaired := strings.ReplaceAll(string(data),
		"상황 1: 전달할 느낌을 골라요.",
		"상황 1: 상사에게 올리는 공식 보고서에서 낮은 가격만 중립적으로 알려야 해요.")
	if repaired == string(data) {
		t.Fatal("fixture context not found")
	}
	judgeCalls := 0
	var auditInput, clarityRepairSystem, clarityRepairInput string
	p := &Pipeline{
		Analysis: []Candidate{{LLM: &fakeLLM{complete: func([]llm.Message) (string, error) { return string(data), nil }}}},
		Judge: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			judgeCalls++
			switch judgeCalls {
			case 1:
				return string(data), nil
			case 2:
				auditInput = msgs[1].Content
				return `{"issues":[{"questionId":"q2","problem":"the context states no register cue"}]}`, nil
			default:
				clarityRepairSystem, clarityRepairInput = msgs[0].Content, msgs[1].Content
				return repaired, nil
			}
		}},
		JudgeModel: "judge",
	}
	c, err := p.GenerateNuance(context.Background(), "likes travel", nil, "draw-1")
	if err != nil {
		t.Fatal(err)
	}
	if judgeCalls != 3 {
		t.Fatalf("Judge calls = %d, want cascade final + clarity audit + one repair", judgeCalls)
	}
	if want := "상황 1: 상사에게 올리는 공식 보고서에서 낮은 가격만 중립적으로 알려야 해요."; c.Questions[1].Context != want {
		t.Fatalf("audited question not replaced with repaired lesson: %+v", c.Questions[1])
	}
	// The auditor sees every server-assigned ID and every alternative word.
	for _, want := range []string{`"q2"`, "cheap", "inexpensive", "Item 1 is ____."} {
		if !strings.Contains(auditInput, want) {
			t.Errorf("audit input missing %q: %s", want, auditInput)
		}
	}
	// The repair carries the authoritative task, the quoted report, and the
	// original request input so regeneration stays within the draw contract.
	if !strings.Contains(clarityRepairSystem, "Repair ambiguous") || !strings.Contains(clarityRepairSystem, "ORIGINAL TASK") {
		t.Fatal("clarity repair system prompt lost its authoritative task")
	}
	for _, want := range []string{"Auditor-reported questions", "the context states no register cue", "draw-1", `"q2"`} {
		if !strings.Contains(clarityRepairInput, want) {
			t.Errorf("clarity repair input missing %q: %s", want, clarityRepairInput)
		}
	}
}

func TestGenerateNuanceKeepsValidLessonWhenClarityStepFails(t *testing.T) {
	data, err := os.ReadFile("../nuance/testdata/lesson.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, failure := range []struct {
		name             string
		audit, repair    string
		auditErr, repErr error
	}{
		{"audit transport error", "", "", errors.New("judge unavailable"), nil},
		{"audit malformed", `not json`, "", nil, nil},
		{"repair transport error", `{"issues":[{"questionId":"q1","problem":"vague"}]}`, "", nil, errors.New("judge unavailable")},
		{"repair invalid lesson", `{"issues":[{"questionId":"q1","problem":"vague"}]}`, `{}`, nil, nil},
	} {
		t.Run(failure.name, func(t *testing.T) {
			judgeCalls := 0
			p := &Pipeline{
				Analysis: []Candidate{{LLM: &fakeLLM{complete: func([]llm.Message) (string, error) { return string(data), nil }}}},
				Judge: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
					judgeCalls++
					switch judgeCalls {
					case 1:
						return string(data), nil
					case 2:
						return failure.audit, failure.auditErr
					default:
						return failure.repair, failure.repErr
					}
				}},
				JudgeModel: "judge",
			}
			c, err := p.GenerateNuance(context.Background(), "", nil, "id")
			if err != nil {
				t.Fatalf("a failed clarity step must not fail a valid lesson: %v", err)
			}
			if len(c.Questions) != 5 || c.Questions[0].Context != "상황 0: 전달할 느낌을 골라요." {
				t.Fatalf("original lesson not kept: %+v", c)
			}
		})
	}
}

func TestSupplementNuanceAuditsAndRepairsAmbiguousQuestions(t *testing.T) {
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
	first := `{"questions":[{"context":"가격표 상황","sentence":"This option is ____.","translation":"이 선택지는 저렴합니다.","answer":"inexpensive","explanation":"inexpensive는 중립적이고 cheap은 품질에 부정적인 인상을 더할 수 있어요."}]}`
	improved := strings.ReplaceAll(first, "가격표 상황", "상사에게 드리는 공식 보고서에서 낮은 가격을 중립적으로 알려야 하는 상황")
	judgeCalls := 0
	var auditInput, clarityRepairInput string
	p := &Pipeline{
		Analysis: []Candidate{{LLM: &fakeLLM{complete: func([]llm.Message) (string, error) { return first, nil }}}},
		Judge: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			judgeCalls++
			switch judgeCalls {
			case 1:
				return first, nil
			case 2:
				auditInput = msgs[1].Content
				return `{"issues":[{"questionId":"s1","problem":"no register cue distinguishes inexpensive"}]}`, nil
			default:
				clarityRepairInput = msgs[1].Content
				return improved, nil
			}
		}},
		JudgeModel: "judge",
	}
	questions, err := p.SupplementNuance(context.Background(), lesson, "supplement-1")
	if err != nil {
		t.Fatal(err)
	}
	if judgeCalls != 3 {
		t.Fatalf("Judge calls = %d, want cascade final + clarity audit + one repair", judgeCalls)
	}
	if len(questions) != 1 || questions[0].Context != "상사에게 드리는 공식 보고서에서 낮은 가격을 중립적으로 알려야 하는 상황" || questions[0].ID != "" {
		t.Fatalf("audited supplement not replaced with repaired questions: %+v", questions)
	}
	for _, want := range []string{`"s1"`, "cheap", "inexpensive", "This option is ____."} {
		if !strings.Contains(auditInput, want) {
			t.Errorf("audit input missing %q: %s", want, auditInput)
		}
	}
	for _, want := range []string{"Auditor-reported questions", "no register cue distinguishes inexpensive", "supplement-1", "requiredAnswers"} {
		if !strings.Contains(clarityRepairInput, want) {
			t.Errorf("clarity repair input missing %q: %s", want, clarityRepairInput)
		}
	}
}

func TestSupplementNuanceKeepsQuestionsWhenClarityStepFails(t *testing.T) {
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
	supplement := `{"questions":[{"context":"중립적인 가격표 상황","sentence":"This option is ____.","translation":"이 선택지는 저렴합니다.","answer":"inexpensive","explanation":"inexpensive는 중립적이고 cheap은 품질에 부정적인 인상을 더할 수 있어요."}]}`
	for _, failure := range []struct {
		name             string
		audit, repair    string
		auditErr, repErr error
	}{
		{"audit transport error", "", "", errors.New("judge unavailable"), nil},
		{"repair invalid supplement", `{"issues":[{"questionId":"s1","problem":"vague"}]}`, `{"questions":[{"context":"b","sentence":"This option is ____.","translation":"b","answer":"cheap","explanation":"b"}]}`, nil, nil},
	} {
		t.Run(failure.name, func(t *testing.T) {
			judgeCalls := 0
			p := &Pipeline{
				Analysis: []Candidate{{LLM: &fakeLLM{complete: func([]llm.Message) (string, error) { return supplement, nil }}}},
				Judge: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
					judgeCalls++
					switch judgeCalls {
					case 1:
						return supplement, nil
					case 2:
						return failure.audit, failure.auditErr
					default:
						return failure.repair, failure.repErr
					}
				}},
				JudgeModel: "judge",
			}
			questions, err := p.SupplementNuance(context.Background(), lesson, "supplement-1")
			if err != nil {
				t.Fatalf("a failed clarity step must not fail a valid supplement: %v", err)
			}
			if len(questions) != 1 || questions[0].Context != "중립적인 가격표 상황" {
				t.Fatalf("original supplement not kept: %+v", questions)
			}
		})
	}
}

func TestDecodeNuanceClarityIssues(t *testing.T) {
	for _, tt := range []struct {
		name, raw string
		want      []nuanceClarityIssue
		wantErr   bool
	}{
		{"clear", `{"issues":[]}`, []nuanceClarityIssue{}, false},
		{"fenced prose", "```json\n{\"issues\":[]}\n```", []nuanceClarityIssue{}, false},
		{"reported", `{"issues":[{"questionId":" q1 ","problem":" vague "}]}`, []nuanceClarityIssue{{QuestionID: "q1", Problem: "vague"}}, false},
		{"drops incomplete entries", `{"issues":[{"questionId":"q1"},{"problem":"x"},{"questionId":"q2","problem":"ok"}]}`, []nuanceClarityIssue{{QuestionID: "q2", Problem: "ok"}}, false},
		{"malformed", `not json`, nil, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			issues, err := decodeNuanceClarityIssues(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("decodeNuanceClarityIssues(%q) unexpectedly succeeded: %+v", tt.raw, issues)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(issues, tt.want) {
				t.Fatalf("decodeNuanceClarityIssues(%q) = (%+v, %v), want %+v", tt.raw, issues, err, tt.want)
			}
		})
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
