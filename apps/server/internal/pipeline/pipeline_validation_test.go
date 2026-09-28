package pipeline

import (
	"context"
	"errors"
	"os"
	"testing"

	"buddy/server/internal/checkpoint"
	"buddy/server/internal/checkpoint/checkpointtest"
	"buddy/server/internal/llm"
	"buddy/server/internal/protocol"
)

func TestGenerationRetryRegeneratesInvalidTerminalOutput(t *testing.T) {
	nuanceData, err := os.ReadFile("../nuance/testdata/lesson.json")
	if err != nil {
		t.Fatal(err)
	}
	issues := []StudyIssue{{Text: "He go.", Issue: protocol.Issue{Type: "grammar"}}}
	for _, tc := range []struct {
		name        string
		invalid     string
		valid       string
		repairFails bool
		generate    func(context.Context, *Pipeline) error
	}{
		{"writing malformed JSON", `not json`, `{"korean":"새 문장"}`, false, func(ctx context.Context, p *Pipeline) error {
			_, err := p.GenerateWritingPrompt(ctx, "profile", nil, "draw")
			return err
		}},
		{"writing empty sentence", `{"korean":" "}`, `{"korean":"새 문장"}`, false, func(ctx context.Context, p *Pipeline) error {
			_, err := p.GenerateWritingPrompt(ctx, "profile", nil, "draw")
			return err
		}},
		{"article missing fields", `{"summary":"Summary"}`, fakeArticleStudyJSON, false, func(ctx context.Context, p *Pipeline) error {
			_, err := p.GenerateArticleStudy(ctx, "source", "title", "description")
			return err
		}},
		{"study summary empty", `{"sentences":[]}`, `{"sentences":[{"english":"Practice agreement.","translation":"수일치를 연습하세요."}]}`, false, func(ctx context.Context, p *Pipeline) error {
			sentences, err := p.GenerateStudySummary(ctx, issues)
			if err == nil && len(sentences) == 0 {
				return errors.New("transport rejects an empty summary for flagged issues")
			}
			return err
		}},
		{"study quiz malformed questions", `{"questions":7}`, `{"questions":[{"prompt":"He ___ home.","answer":"goes"}]}`, false, func(ctx context.Context, p *Pipeline) error {
			_, err := p.GenerateStudyQuiz(ctx, issues)
			return err
		}},
		{"word question incomplete blank", `{"prompt":"He ___s home.","answers":["go"]}`, `{"prompt":"He ___ home.","answers":["goes"]}`, false, func(ctx context.Context, p *Pipeline) error {
			_, err := p.GenerateWordReviewQuestion(ctx, "go", "가다", "He goes home.")
			return err
		}},
		{"word verification missing verdict", `{"reason":"No verdict"}`, `{"valid":true}`, false, func(ctx context.Context, p *Pipeline) error {
			_, _, err := p.VerifyWord(ctx, "go", "가다", "He goes home.")
			return err
		}},
		{"word meaning uncertain", `{"sameSense":false,"meaning":""}`, `{"sameSense":true,"meaning":"가다"}`, false, func(ctx context.Context, p *Pipeline) error {
			_, err := p.NormalizeWordMeaning(ctx, "go", "가다", "He goes home.")
			return err
		}},
		{"word suggestions malformed entry", `{"suggestions":[{"word":7}]}`, `{"suggestions":[{"word":"go","meaning":"가다","example":"He goes home."}]}`, false, func(ctx context.Context, p *Pipeline) error {
			_, err := p.SuggestNewWords(ctx, "profile", nil)
			return err
		}},
		{"correction malformed issues", `{"issues":7}`, `{"corrected":"He goes.","issues":[]}`, false, func(ctx context.Context, p *Pipeline) error {
			_, _, _, err := p.AnalyzeCorrection(ctx, "He go.", "")
			return err
		}},
		{"nuance invalid lesson with interrupted repair", `{}`, string(nuanceData), true, func(ctx context.Context, p *Pipeline) error {
			_, err := p.GenerateNuance(ctx, "profile", nil, "draw")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var saved checkpointtest.Memory
			drafts, refinements, judgements := 0, 0, 0
			newPipeline := func() *Pipeline {
				return &Pipeline{
					ChatModel: "chat", LLM: &fakeLLM{complete: func([]llm.Message) (string, error) {
						drafts++
						return tc.valid, nil
					}},
					Analysis: []Candidate{{Model: "analysis", LLM: &fakeLLM{complete: func([]llm.Message) (string, error) {
						refinements++
						return tc.valid, nil
					}}}},
					JudgeModel: "judge", Judge: &fakeLLM{complete: func([]llm.Message) (string, error) {
						judgements++
						if judgements == 1 {
							return tc.invalid, nil
						}
						if judgements == 2 && tc.repairFails {
							return "", errors.New("repair interrupted")
						}
						return tc.valid, nil
					}},
				}
			}
			if err := tc.generate(checkpoint.Bind(context.Background(), &saved), newPipeline()); err == nil {
				t.Fatal("invalid terminal response unexpectedly succeeded")
			}
			if err := tc.generate(checkpoint.Bind(context.Background(), &saved), newPipeline()); err != nil {
				t.Fatalf("retry reused rejected terminal response: %v", err)
			}
			wantJudgements := 2
			if tc.repairFails {
				wantJudgements++
			}
			if drafts != 1 || refinements != 1 || judgements != wantJudgements {
				t.Fatalf("calls = chat:%d analysis:%d judge:%d; want 1,1,%d", drafts, refinements, judgements, wantJudgements)
			}
		})
	}
}
