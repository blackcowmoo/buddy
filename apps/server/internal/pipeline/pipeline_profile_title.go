package pipeline

import (
	"context"
	"log"
	"strings"

	"buddy/server/internal/llm"
)

// UpdateLearnerProfile folds one just-ended session's study wrap-up (see
// GenerateStudySummary, written in the learner's native language) into
// userID's persistent, cross-session profile (store.Store.GetLearnerProfile/
// SaveLearnerProfile) — the same "roll the old summary and new material into
// one updated summary" shape compact() uses for a single session's own
// long-term memory, except this one spans every conversation the learner has
// ever ended, not just turns within one room. Called once, from
// httpserver.sessionEndHandler, when the learner confirms "end this
// conversation" — never on every reload.
func (p *Pipeline) UpdateLearnerProfile(ctx context.Context, prevProfile, sessionSummary string) (string, error) {
	raw, err := p.analyze(ctx, learnerProfileSystemPrompt, renderLearnerProfileInput(prevProfile, sessionSummary), false)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(raw), nil
}

// learnerProfileSystemPrompt is written in English and asks for an
// English-only result: unlike GenerateStudySummary's output (shown directly
// to the learner, so it's in their native language), this profile is only
// ever machine-consumed — folded into BuildSystemPrompt's system prompt for
// a brand-new session — the same reasoning compactionSystemPrompt uses.
const learnerProfileSystemPrompt = `You maintain a persistent, cross-conversation profile of an
English-learning student, used as background context in future practice sessions.
Given the previous profile (may be empty) and a new wrap-up note from a conversation
that was just ended (written in the learner's native language), write ONE updated
profile that:
- Preserves and updates useful long-term facts: interests, goals, proficiency level,
  and recurring grammar/vocabulary/phrasing patterns the learner should keep working on.
- Stays concise (a few sentences to a short paragraph) — it is background context for
  an LLM, not a report to show the learner.
- Is written in English, regardless of what language the new note is in.
Return ONLY the updated profile text. No labels, no JSON, no preamble.`

func renderLearnerProfileInput(prevProfile, sessionSummary string) string {
	var b strings.Builder
	b.WriteString("Previous profile:\n")
	if prevProfile == "" {
		b.WriteString("(none)\n")
	} else {
		b.WriteString(prevProfile + "\n")
	}
	b.WriteString("\nNew session wrap-up to fold in:\n")
	b.WriteString(sessionSummary)
	return b.String()
}

// RunTitle asks for a chat-room title from transcript and saves it, either
// via TitleHook (durable, queue-backed — see transport.NewTitleHook) or by
// calling GenerateTitle and save directly in-process — same
// hook-if-set/direct-otherwise shape as runReply/correct/translateAssistant.
// Called by internal/transport on turn 1 and every TitleRegenerateEveryNTurns
// turns after that (see Handler.generateTitle), not from Pipeline's own
// turn-handling, since triggering it depends on transport's turn-counting
// convention, not anything Pipeline itself tracks. save persists the
// generated title (there's no ServerEvent for it — the frontend just
// re-fetches the room list — so unlike the other hooks' onResult, this
// exists purely for the direct/no-hook fallback; TitleHook is responsible
// for persisting on its own path, see transport.TitleJobHandler).
func (p *Pipeline) RunTitle(ctx context.Context, userID, sessionID string, turn int, transcript []llm.Message, save func(title string) error) {
	if p.TitleHook != nil {
		p.TitleHook(ctx, userID, sessionID, turn, transcript)
		return
	}
	title, err := p.GenerateTitle(ctx, transcript)
	if err != nil {
		log.Printf("title: generate %s/%s: %v", userID, sessionID, err)
		return
	}
	if title = strings.TrimSpace(title); title == "" {
		return
	}
	if err := save(title); err != nil {
		log.Printf("title: save %s/%s: %v", userID, sessionID, err)
	}
}

// GenerateTitle asks the LLM for a short, descriptive chat-room title from
// the conversation so far — called by RunTitle above (directly, or from
// within transport.TitleJobHandler when TitleHook is set) — replacing the
// raw truncated-first-message placeholder store.MySQLStore.SaveTurn sets on
// turn 1, and again every TitleRegenerateEveryNTurns turns after that, so
// the title keeps tracking the conversation's actual topic rather than
// staying pinned to turn 1's. A single fast call (p.LLM/p.ChatModel), not
// the Analysis ensemble+Judge like analyze() uses: a room label is
// decorative, not something a learner's grammar feedback depends on, so it
// isn't worth doubling the LLM cost of every title (re)generation.
func (p *Pipeline) GenerateTitle(ctx context.Context, transcript []llm.Message) (string, error) {
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: titleSystemPrompt},
		{Role: llm.RoleUser, Content: renderTitleInput(transcript)},
	}
	title, err := p.LLM.Complete(ctx, p.ChatModel, msgs, false)
	if err != nil {
		return "", err
	}
	// Small models like to wrap a short title in quotes despite being told
	// not to; strip them so the stored title doesn't carry literal "" marks.
	return strings.Trim(strings.TrimSpace(title), `"“”`), nil
}

const titleSystemPrompt = `Give a short, descriptive title for a chat conversation, based on
the conversation so far (it may be just the opening exchange, or many turns
in). The title summarizes the current TOPIC being discussed, not the
learner's exact words.
Rules:
- 2-6 words.
- Plain text: no quotes, no trailing punctuation, no labels like "Title:".
- Write it in the same language the learner used.
- Return ONLY the title, nothing else.`

func renderTitleInput(transcript []llm.Message) string {
	var b strings.Builder
	writeTranscript(&b, transcript)
	return b.String()
}
