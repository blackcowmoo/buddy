package transport

import (
	"context"
	"testing"

	"buddy/server/internal/pipeline"
	"buddy/server/internal/protocol"
	"buddy/server/internal/wordreview"
)

// generatedSavingFakeStore approximates wordreview.MySQLStore's optional
// GeneratedSaver: freshly saved rows come back stamped at the current meaning
// contract, exactly like SaveGenerated writes them.
type generatedSavingFakeStore struct {
	*fakeWordReviewStore
	generatedCalls int
}

// SaveGenerated mirrors SaveGenerated's dedup rule: a duplicate triple keeps
// the existing row untouched (which may be a legacy unversioned row), so only
// a freshly created row receives the contract stamps.
func (s *generatedSavingFakeStore) SaveGenerated(ctx context.Context, userID, word, meaning, example, originalWord string) (wordreview.Word, error) {
	s.mu.Lock()
	for _, w := range s.words {
		if w.UserID == userID && w.Word == word && w.Meaning == meaning {
			s.generatedCalls++
			s.mu.Unlock()
			return w, nil
		}
	}
	s.mu.Unlock()
	saved, err := s.fakeWordReviewStore.Save(ctx, userID, word, meaning, example)
	if err != nil {
		return saved, err
	}
	saved.MeaningVersion = wordreview.CurrentMeaningVersion
	saved.MeaningStatus = wordreview.MeaningConfirmed
	saved.ResearchStatus = wordreview.ResearchConfirmed
	s.mu.Lock()
	s.words[saved.ID] = saved
	s.generatedCalls++
	s.mu.Unlock()
	return saved, nil
}

// SaveWordAndVerify's meanings always come from prompts embedding the current
// dictionary-meaning contract (word popup, DefineWord, auto-add), so a store
// that can stamp them must be used instead of the plain Save/SaveOriginal
// path — otherwise every new word would wait for a cleanup pass it cannot
// fail without forcing the learner through a pointless choice.
func TestSaveWordAndVerifyStampsContractGeneratedMeanings(t *testing.T) {
	store := &generatedSavingFakeStore{fakeWordReviewStore: newFakeWordReviewStore()}
	saved, err := SaveWordAndVerify(context.Background(), store, &pipeline.Pipeline{}, nil, "alex", "furious", "몹시 화난", "She was furious.", "Furious")
	if err != nil {
		t.Fatal(err)
	}
	if store.generatedCalls != 1 {
		t.Fatalf("SaveGenerated calls = %d, want the stamped path", store.generatedCalls)
	}
	if saved.MeaningVersion != wordreview.CurrentMeaningVersion || saved.MeaningStatus != wordreview.MeaningConfirmed || saved.ResearchStatus != wordreview.ResearchConfirmed {
		t.Fatalf("saved = %+v, want stamped at the current meaning contract", saved)
	}
}

// A correction's studyMeaning is contract-generated and gets stamped; a
// legacy queued correction without the field falls back to the explanation
// translation, which was never contract-checked and must stay unversioned so
// the cleanup pass assesses it.
func TestCaptureCorrectionWordsStampsStudyMeaningOnly(t *testing.T) {
	store := &generatedSavingFakeStore{fakeWordReviewStore: newFakeWordReviewStore()}
	c := protocol.Correction{
		Corrected: "She was furious about the postponement.",
		Issues: []protocol.Issue{
			{Type: "vocabulary", Span: "very angry", Suggestion: "furious", StudyMeaning: "몹시 화난", ExplanationTranslation: "더 강한 감정"},
			{Type: "vocabulary", Span: "delay", Suggestion: "postponement", ExplanationTranslation: "연기"},
		},
	}
	captureCorrectionWords(context.Background(), &pipeline.Pipeline{}, store, nil, "alex", c)
	list, err := store.List(context.Background(), "alex")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("list = %+v, want both vocabulary issues captured", list)
	}
	if store.generatedCalls != 1 {
		t.Fatalf("SaveGenerated calls = %d, want only the studyMeaning row", store.generatedCalls)
	}
	for _, w := range list {
		switch w.Word {
		case "furious":
			if w.MeaningVersion != wordreview.CurrentMeaningVersion || w.MeaningStatus != wordreview.MeaningConfirmed {
				t.Errorf("studyMeaning row unstamped: %+v", w)
			}
		case "postponement":
			if w.Meaning != "연기" || w.MeaningVersion != 0 || w.MeaningStatus != "" {
				t.Errorf("legacy fallback row wrongly stamped: %+v", w)
			}
		default:
			t.Errorf("unexpected word captured: %+v", w)
		}
	}
}
