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
	"log"
	"strings"

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

type Pipeline struct {
	FastSTT      stt.Recognizer
	SlowSTT      stt.Recognizer
	LLM          llm.Client
	ChatModel    string
	CorrectModel string
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

// correct asks the LLM for grammar/vocabulary feedback as strict JSON.
func (p *Pipeline) correct(ctx context.Context, turn int, text string, emit Emit) {
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: correctionSystemPrompt},
		{Role: llm.RoleUser, Content: text},
	}
	raw, err := p.LLM.Complete(ctx, p.CorrectModel, msgs, true)
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

const correctionSystemPrompt = `You are an English writing coach for a language learner.
The input is one spoken sentence, possibly with speech-to-text noise.
Return STRICT JSON only, no prose, in exactly this shape:
{"corrected":"<the sentence rewritten in correct, natural English>",
 "issues":[{"type":"grammar|vocabulary|phrasing","span":"<original text>","suggestion":"<fix>","explanation":"<short, kind, learner-friendly>"}]}
If the sentence is already correct, return the same text and an empty issues array.`

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
