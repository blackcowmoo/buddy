package transport

import (
	"context"
	"fmt"
	"strings"

	"buddy/server/internal/newsarticle"
	"buddy/server/internal/pipeline"
)

// RunArticleTranslationBackfill fills the translation for an older article
// generated before translations were saved. Claiming is atomic in the store,
// so repeated article polling is safe and only one local-LLM call is made.
func RunArticleTranslationBackfill(ctx context.Context, pipe *pipeline.Pipeline, articles newsarticle.Store, articleID string) error {
	backfiller, ok := articles.(newsarticle.ArticleTranslationBackfiller)
	if !ok {
		return nil
	}
	a, found, err := articles.GetArticle(ctx, articleID)
	if err != nil || !found || a.Status != newsarticle.StatusDone || strings.TrimSpace(a.Translation) != "" {
		return err
	}
	claimed, err := backfiller.ClaimMissingTranslation(ctx, articleID)
	if err != nil || !claimed {
		return err
	}
	translation, err := pipe.AnalyzeTranslation(ctx, a.Summary)
	if err != nil {
		return fmt.Errorf("article translation: generate: %w", err)
	}
	if err := backfiller.CompleteArticleTranslation(ctx, articleID, translation); err != nil {
		return fmt.Errorf("article translation: save: %w", err)
	}
	return nil
}
