// Package pipeline orchestrates one conversational turn across two tracks:
//
//	FAST   : fast STT -> streamed LLM reply  (low latency, what the user hears)
//	REFINE : slow STT -> grammar/context correction (background, more accurate)
//
// The two tracks share the session so the refine track can upgrade the last
// user turn once the high-quality transcript lands.
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"

	"buddy/server/internal/llm"
	"buddy/server/internal/protocol"
	"buddy/server/internal/session"
	"buddy/server/internal/stt"
)

// Emit delivers an event to the client. It is safe for concurrent use — the
// transport layer serializes writes.
type Emit func(protocol.ServerEvent)

const DefaultSystemPrompt = `You are Buddy, a warm, encouraging English conversation partner.
Keep the conversation flowing naturally: reply in 1-3 short spoken-style sentences,
ask a follow-up question, and match the learner's level. Do NOT correct grammar
inline — corrections are handled separately. Never mention that you are an AI.`

// Candidate is one ensemble member consulted during REFINE-track analysis
// (grammar correction, context compaction): a model and the endpoint that
// serves it. See Pipeline.Analysis and analyze().
type Candidate struct {
	LLM   llm.Client
	Model string
}

type Pipeline struct {
	FastSTT stt.Recognizer
	SlowSTT stt.Recognizer

	// LLM/ChatModel: FAST track's streamed reply — one model, low latency.
	LLM       llm.Client
	ChatModel string

	// Analysis: REFINE track's grammar-correction/compaction pass. Every
	// candidate is asked concurrently (analyze()); a lone candidate is used
	// directly, and two or more are synthesized by Judge. This is how
	// multiple local models (e.g. several checkpoints behind llama.cpp) get
	// combined into one higher-confidence result instead of picking just one.
	Analysis []Candidate
	// Judge synthesizes the Analysis ensemble's outputs into the single
	// result analyze() returns. Unused (and may be nil) when len(Analysis)<=1.
	Judge      llm.Client
	JudgeModel string

	// FeedbackLang is the learner's native language for correction
	// explanations (e.g. "ko"). The corrected sentence stays in English.
	FeedbackLang string
	// MaxHistoryMessages caps the verbatim window kept in session.Session
	// before the oldest half is folded into its long-term summary. See
	// compact() and session.PeekOldestForCompaction.
	MaxHistoryMessages int
}

// HandleUtterance runs one turn from raw audio.
func (p *Pipeline) HandleUtterance(ctx context.Context, sess *session.Session, pcm []byte, emit Emit) {
	turn := sess.NextTurn()

	// --- FAST STT --------------------------------------------------------
	fast, err := p.FastSTT.Transcribe(ctx, pcm)
	if err != nil {
		emit(protocol.ServerEvent{Type: protocol.EvError, Turn: turn, Text: "stt: " + err.Error()})
		return
	}
	userText := strings.TrimSpace(fast.Text)
	if userText == "" {
		return
	}
	emit(protocol.ServerEvent{Type: protocol.EvFinal, Turn: turn, Text: userText})
	sess.AppendUser(userText)

	// --- REFINE track (background) --------------------------------------
	go p.refine(ctx, sess, turn, pcm, userText, emit)

	// --- FAST reply ------------------------------------------------------
	p.reply(ctx, sess, turn, emit)
}

// HandleText runs one turn from typed input (skips STT).
func (p *Pipeline) HandleText(ctx context.Context, sess *session.Session, text string, emit Emit) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	turn := sess.NextTurn()
	emit(protocol.ServerEvent{Type: protocol.EvFinal, Turn: turn, Text: text})
	sess.AppendUser(text)
	go p.correct(ctx, turn, text, emit) // correction only; no slow STT needed
	p.reply(ctx, sess, turn, emit)
}

// reply streams the assistant response and records it in the session.
func (p *Pipeline) reply(ctx context.Context, sess *session.Session, turn int, emit Emit) {
	msgs := sess.Snapshot()
	full, err := p.LLM.ChatStream(ctx, p.ChatModel, msgs, func(tok string) {
		emit(protocol.ServerEvent{Type: protocol.EvAssistantDelta, Turn: turn, Text: tok})
	})
	// Barged in (user spoke again): drop this turn's tail, keep history clean.
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		log.Printf("chat: %v", err)
		if strings.TrimSpace(full) == "" {
			full = fallbackReply(msgs)
			emit(protocol.ServerEvent{Type: protocol.EvAssistantDelta, Turn: turn, Text: full})
		}
	}
	emit(protocol.ServerEvent{Type: protocol.EvAssistantDone, Turn: turn, Text: full})
	sess.AppendAssistant(full)
	go p.compact(sess) // background: fold old turns into the long-term summary
}

// analyze runs one REFINE-track task (grammar correction or compaction)
// across every Analysis candidate concurrently. A single successful
// candidate is returned as-is — nothing to synthesize. Two or more are
// handed to Judge, which picks/merges them into the one final answer; if
// Judge itself fails, the first candidate's answer is used so a flaky judge
// degrades gracefully instead of losing the turn.
func (p *Pipeline) analyze(ctx context.Context, systemPrompt, input string, jsonMode bool) (string, error) {
	if len(p.Analysis) == 0 {
		return "", fmt.Errorf("analyze: no candidates configured")
	}

	type candidateResult struct {
		model string
		text  string
		ok    bool
	}
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: systemPrompt},
		{Role: llm.RoleUser, Content: input},
	}

	// Each goroutine owns a fixed slot by index, so the results slice stays
	// in Analysis's configured order regardless of completion timing — the
	// judge-error fallback below always means "the first configured
	// candidate", not "whichever happened to finish first".
	slots := make([]candidateResult, len(p.Analysis))
	var wg sync.WaitGroup
	for i, c := range p.Analysis {
		wg.Add(1)
		go func(i int, c Candidate) {
			defer wg.Done()
			text, err := c.LLM.Complete(ctx, c.Model, msgs, jsonMode)
			if err != nil {
				log.Printf("analyze: candidate %s: %v", c.Model, err)
				return
			}
			if text = strings.TrimSpace(text); text == "" {
				return
			}
			slots[i] = candidateResult{model: c.Model, text: text, ok: true}
		}(i, c)
	}
	wg.Wait()

	var results []candidateResult
	for _, r := range slots {
		if r.ok {
			results = append(results, r)
		}
	}

	if len(results) == 0 {
		return "", fmt.Errorf("analyze: every candidate failed")
	}
	if len(results) == 1 {
		return results[0].text, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Original task:\n%s\n\nOriginal input:\n%s\n\n", systemPrompt, input)
	for _, r := range results {
		fmt.Fprintf(&b, "--- candidate (%s) ---\n%s\n\n", r.model, r.text)
	}
	judgeMsgs := []llm.Message{
		{Role: llm.RoleSystem, Content: judgeSystemPrompt},
		{Role: llm.RoleUser, Content: b.String()},
	}
	final, err := p.Judge.Complete(ctx, p.JudgeModel, judgeMsgs, jsonMode)
	if err != nil {
		log.Printf("analyze: judge: %v; falling back to first candidate", err)
		return results[0].text, nil
	}
	if final = strings.TrimSpace(final); final == "" {
		return results[0].text, nil
	}
	return final, nil
}

const judgeSystemPrompt = `Several candidate models independently performed the same task below.
Synthesize them into the single best final answer, following the ORIGINAL
task's instructions and required output format EXACTLY (e.g. if it asked for
strict JSON, output strict JSON and nothing else). Output ONLY the final
answer — no preamble, no meta-commentary about the candidates or the judging
process.`

// compact folds the oldest verbatim turns into the session's long-term
// summary once the window exceeds MaxHistoryMessages, so long conversations
// stay cheap to send to the LLM and to persist (internal/store). It uses
// context.Background() — bookkeeping on already-committed history, so a
// barge-in on the current turn must not cancel it.
func (p *Pipeline) compact(sess *session.Session) {
	old, curSummary, ok := sess.PeekOldestForCompaction(p.MaxHistoryMessages)
	if !ok {
		return
	}
	newSummary, err := p.analyze(context.Background(), compactionSystemPrompt, renderCompactionInput(curSummary, old), false)
	if err != nil {
		log.Printf("compact: %v", err) // leave history untouched; retried next turn
		return
	}
	sess.ApplyCompaction(strings.TrimSpace(newSummary), len(old))
}

const compactionSystemPrompt = `You maintain compact, persistent memory of an English-learning conversation.
Given the previous summary (may be empty) and a batch of older verbatim turns,
write ONE updated summary that:
- Preserves useful long-term facts about the learner: interests, goals, recurring
  grammar/vocabulary mistakes, proficiency level, and topics already discussed.
- Stays concise (a few sentences to a short paragraph) — this replaces the raw
  turns, it is not a transcript.
- Is written in English.
Return ONLY the updated summary text. No labels, no JSON, no preamble.`

func renderCompactionInput(prevSummary string, old []llm.Message) string {
	var b strings.Builder
	b.WriteString("Previous summary:\n")
	if prevSummary == "" {
		b.WriteString("(none)\n")
	} else {
		b.WriteString(prevSummary + "\n")
	}
	b.WriteString("\nOlder turns to fold in:\n")
	for _, m := range old {
		fmt.Fprintf(&b, "%s: %s\n", m.Role, m.Content)
	}
	return b.String()
}

// refine re-transcribes with the slow/quality engine, upgrades the session
// context, then runs the correction pass.
func (p *Pipeline) refine(ctx context.Context, sess *session.Session, turn int, pcm []byte, fastText string, emit Emit) {
	slow, err := p.SlowSTT.Transcribe(ctx, pcm)
	refined := fastText
	if err != nil {
		log.Printf("slow stt: %v", err)
	} else if t := strings.TrimSpace(slow.Text); t != "" {
		refined = t
		emit(protocol.ServerEvent{Type: protocol.EvRefined, Turn: turn, Text: refined})
		sess.ReplaceLastUser(refined) // keep future context accurate
	}
	p.correct(ctx, turn, refined, emit)
}

// correct asks the analysis ensemble for grammar/vocabulary feedback as
// strict JSON, synthesized down to one result by analyze().
func (p *Pipeline) correct(ctx context.Context, turn int, text string, emit Emit) {
	raw, err := p.analyze(ctx, correctionSystemPrompt(p.FeedbackLang), text, true)
	if err != nil {
		log.Printf("correct: %v", err)
		return
	}
	var parsed struct {
		Corrected string           `json:"corrected"`
		Issues    []protocol.Issue `json:"issues"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		log.Printf("correct: bad json: %v", err)
		return
	}
	// Nothing to teach — skip the card.
	if strings.EqualFold(strings.TrimSpace(parsed.Corrected), strings.TrimSpace(text)) && len(parsed.Issues) == 0 {
		return
	}
	emit(protocol.ServerEvent{
		Type: protocol.EvCorrection,
		Turn: turn,
		Correction: &protocol.Correction{
			Original:  text,
			Corrected: parsed.Corrected,
			Issues:    parsed.Issues,
		},
	})
}

// correctionSystemPrompt builds the grammar-coach prompt. The corrected
// sentence, span, and suggestion stay in English (the language being learned);
// only the explanation is written in the learner's native language so the
// feedback is easy to understand.
func correctionSystemPrompt(lang string) string {
	native := languageName(lang)
	return fmt.Sprintf(`You are an English writing coach for a %[1]s-speaking learner.
The input is one spoken English sentence, possibly with speech-to-text noise.
Return STRICT JSON only, no prose, in exactly this shape:
{"corrected":"<the sentence rewritten in correct, natural English>",
 "issues":[{"type":"grammar|vocabulary|phrasing","span":"<original English text>","suggestion":"<the English fix>","explanation":"<why it is wrong, written in %[1]s, short and kind>"}]}
Rules:
- "corrected", "span", and "suggestion" MUST stay in English.
- "explanation" MUST be written in %[1]s.
- If the sentence is already correct, return the same text and an empty issues array.`, native)
}

// languageName maps a short language code to an English name the LLM
// understands. Unknown codes fall back to the code itself.
func languageName(code string) string {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "ko", "ko-kr":
		return "Korean"
	case "en", "en-us":
		return "English"
	case "ja", "ja-jp":
		return "Japanese"
	case "zh", "zh-cn":
		return "Chinese"
	case "es":
		return "Spanish"
	case "", "auto":
		return "Korean"
	default:
		return code
	}
}

func fallbackReply(msgs []llm.Message) string {
	last := ""
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == llm.RoleUser {
			last = msgs[i].Content
			break
		}
	}
	return "(LLM offline) I heard: \"" + last + "\". Tell me more!"
}
