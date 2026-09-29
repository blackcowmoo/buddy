package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"

	"buddy/server/internal/llm"
)

func TestWordResearchRepairsRejectedUsageBeforePublishing(t *testing.T) {
	const oldExample = "The government plans to recapitalize the infrastructure."
	const oldReason = "기업이나 은행에 자본을 투입하는 말이라 인프라를 대상으로 쓰면 부자연스럽습니다."
	generation, verification := 0, 0
	p := &Pipeline{FeedbackLang: "ko", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		system, input := msgs[0].Content, msgs[len(msgs)-1].Content
		switch system {
		case wordFormSystemPrompt():
			if strings.Contains(input, oldExample) {
				t.Fatal("rejected example used as authoritative form-resolution context")
			}
			return `{"word":"recapitalize"}`, nil
		case wordVerifySystemPrompt("ko"):
			verification++
			if strings.Contains(input, "bridges") {
				return `{"valid":false,"reason":"교량 대신 자본이 필요한 기업이나 은행을 대상으로 예문을 만드세요."}`, nil
			}
			if !strings.Contains(input, "recapitalize the bank") {
				t.Fatalf("unexpected candidate: %s", input)
			}
			return `{"valid":true}`, nil
		default:
			generation++
			if !strings.Contains(input, oldReason) || !strings.Contains(input, oldExample) {
				t.Fatal("research lost rejection evidence")
			}
			if generation == 1 {
				return `{"suggestions":[{"word":"recapitalize","meaning":"새 자본을 공급하다","example":" THE government plans to recapitalize the infrastructure! "},{"word":"recapitalize","meaning":"재자본화하다","example":"We plan to recapitalize the bridges."}]}`, nil
			}
			if !strings.Contains(input, "교량 대신") || !strings.Contains(input, "recapitalize the bridges") {
				t.Fatalf("next attempt did not receive the new verdict: %s", input)
			}
			return `{"suggestions":[{"word":"recapitalize","meaning":"재자본화하다","example":"Investors agreed to recapitalize the bank."}]}`, nil
		}
	}}}
	got, err := p.DefineWordMeanings(context.Background(), "recapitalize", "재자본화하다", oldExample, oldReason)
	if err != nil || len(got) != 1 || got[0].Example != "Investors agreed to recapitalize the bank." || generation != 2 || verification != 2 {
		t.Fatalf("results=%+v generations=%d verifications=%d err=%v", got, generation, verification, err)
	}
}

func TestWordResearchStopsRepeatingRejectedExamples(t *testing.T) {
	for _, originalRejected := range []bool{false, true} {
		t.Run(map[bool]string{false: "new rejection", true: "saved rejection"}[originalRejected], func(t *testing.T) {
			generations, verifications := 0, 0
			p := &Pipeline{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
				switch msgs[0].Content {
				case wordFormSystemPrompt():
					return `{"word":"recapitalize"}`, nil
				case wordVerifySystemPrompt(""):
					verifications++
					return `{"valid":false,"reason":"Use a company or bank as the object."}`, nil
				default:
					generations++
					return `{"suggestions":[{"word":"recapitalize","meaning":"a changed gloss","example":"We recapitalize the road."}]}`, nil
				}
			}}}
			reason := ""
			if originalRejected {
				reason = "Use a company or bank as the object."
			}
			got, err := p.DefineWordMeanings(context.Background(), "recapitalize", "재자본화하다", "We recapitalize the road.", reason)
			wantVerifications := 1
			if originalRejected {
				wantVerifications = 0
			}
			if !errors.Is(err, ErrNoVerifiedWordMeanings) || len(got) != 0 || generations != 3 || verifications != wantVerifications {
				t.Fatalf("results=%v generations=%d verifications=%d err=%v", got, generations, verifications, err)
			}
		})
	}
}

func TestWordResearchPublishesOnlyCompleteMatchingVerifiedCandidates(t *testing.T) {
	verifications := 0
	p := &Pipeline{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		switch msgs[0].Content {
		case wordFormSystemPrompt():
			return `{"word":"bank"}`, nil
		case wordVerifySystemPrompt(""):
			verifications++
			if strings.Contains(msgs[len(msgs)-1].Content, "wrong") {
				return `{"valid":false,"reason":"Mismatched sense."}`, nil
			}
			return `{"valid":true}`, nil
		default:
			return `{"suggestions":[
			{"word":"bank","meaning":"은행","example":""},
			{"word":"shore","meaning":"해안","example":"They went to the shore."},
			{"word":"bank","meaning":"은행","example":"wrong use of bank"},
			{"word":"BANK","meaning":" 은행 ","example":" I went to the bank. "},
			{"word":"bank","meaning":"금융 기관","example":"I went to the bank!"}]}`, nil
		}
	}}}
	got, err := p.DefineWordMeanings(context.Background(), "bank", "은행", "", "")
	if err != nil || len(got) != 1 || got[0].Word != "bank" || got[0].Meaning != "은행" || got[0].Example != "I went to the bank." || verifications != 2 {
		t.Fatalf("results=%+v verifications=%d err=%v", got, verifications, err)
	}
}

func TestWordResearchVerificationErrorsNeverPublishCandidates(t *testing.T) {
	for _, verdict := range []string{`{"reason":"looks fine"}`, `{"valid":false}`, `not JSON`, "unavailable"} {
		t.Run(verdict, func(t *testing.T) {
			p := &Pipeline{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
				switch msgs[0].Content {
				case wordFormSystemPrompt():
					return `{"word":"bank"}`, nil
				case wordVerifySystemPrompt(""):
					if verdict == "unavailable" {
						return "", errors.New("model unavailable")
					}
					return verdict, nil
				default:
					return `{"suggestions":[{"word":"bank","meaning":"은행","example":"I went to the bank."}]}`, nil
				}
			}}}
			got, err := p.DefineWordMeanings(context.Background(), "bank", "", "", "")
			if err == nil || errors.Is(err, ErrNoVerifiedWordMeanings) || len(got) != 0 {
				t.Fatalf("unverified content escaped: %+v err=%v", got, err)
			}
		})
	}
}

func TestWordResearchUsesFinalVerificationVerdict(t *testing.T) {
	var stages []string
	model := func(stage string) *fakeLLM {
		return &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			if msgs[0].Content == wordFormSystemPrompt() {
				return `{"word":"bank"}`, nil
			}
			if strings.Contains(msgs[0].Content, "strict fact-checker") {
				stages = append(stages, stage)
				if stage == "judge" {
					return `{"valid":false,"reason":"The example uses the wrong sense."}`, nil
				}
				return `{"valid":true}`, nil
			}
			return `{"suggestions":[{"word":"bank","meaning":"은행","example":"They sat on the river bank."}]}`, nil
		}}
	}
	p := &Pipeline{LLM: model("chat"), Analysis: []Candidate{{LLM: model("analysis")}}, Judge: model("judge")}
	got, err := p.DefineWordMeanings(context.Background(), "bank", "은행", "", "")
	if !errors.Is(err, ErrNoVerifiedWordMeanings) || len(got) != 0 || strings.Join(stages, ",") != "chat,analysis,judge" {
		t.Fatalf("results=%v stages=%v err=%v", got, stages, err)
	}
}

func TestWordResearchRecoversInflectedInputWhenFastResolutionFails(t *testing.T) {
	for _, candidate := range []string{"frill", "decoration"} {
		t.Run(candidate, func(t *testing.T) {
			p := &Pipeline{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
				switch msgs[0].Content {
				case wordFormSystemPrompt():
					return "", errors.New("form model unavailable")
				case wordVerifySystemPrompt(""):
					if !strings.Contains(msgs[len(msgs)-1].Content, "requestedWord: frills") || !strings.Contains(msgs[0].Content, "never a synonym") {
						t.Fatal("fallback verification did not check the requested lexeme")
					}
					if candidate == "decoration" {
						return `{"valid":false,"reason":"A synonym is not the requested word."}`, nil
					}
					return `{"valid":true}`, nil
				default:
					return `{"suggestions":[{"word":"` + candidate + `","meaning":"장식","example":"The dress has a ` + candidate + `."}]}`, nil
				}
			}}}
			got, err := p.DefineWordMeanings(context.Background(), "frills", "장식", "The dress has frills.", "")
			if candidate == "frill" {
				if err != nil || len(got) != 1 || got[0].Word != "frill" {
					t.Fatalf("fallback failed: %+v err=%v", got, err)
				}
			} else if !errors.Is(err, ErrNoVerifiedWordMeanings) || len(got) != 0 {
				t.Fatalf("fallback substituted another word: %+v err=%v", got, err)
			}
		})
	}
}
