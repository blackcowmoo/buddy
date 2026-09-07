package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"buddy/server/internal/llm"
)

// transcribe runs every configured STT engine concurrently on one utterance.
// A lone engine's result (or the zero-setup mock default) is used directly.
// Two or more candidates go to the FAST chat model to reconcile into the
// single sentence the learner most likely said, using the pre-turn
// conversation for context (see synthesizeTranscript) — if that call fails,
// the first engine's candidate is used so a flaky reconciliation never loses
// the turn. The raw candidates are returned too, so HandleUtterance can pass
// them and the exact Chat result through Analysis and Judge without re-running
// STT.
//
// Every engine failing outright is an error (STT is down); every engine
// succeeding but hearing silence is not — it returns ("", candidates, nil),
// same as a single quiet/empty transcript always has.
func (p *Pipeline) transcribe(ctx context.Context, summary string, recent []llm.Message, pcm []byte) (final string, candidates []string, err error) {
	if len(p.STT) == 0 {
		return "", nil, fmt.Errorf("transcribe: no STT engines configured")
	}

	type slot struct {
		text string
		err  error
	}
	slots := fanOutOrdered(len(p.STT), func(i int) slot {
		rec := p.STT[i]
		res, err := rec.Transcribe(ctx, pcm)
		if err != nil {
			log.Printf("transcribe: %s: %v", rec.Name(), err)
			return slot{err: err}
		}
		return slot{text: strings.TrimSpace(res.Text)}
	})

	succeeded := 0
	var errs []error
	for _, s := range slots {
		if s.err != nil {
			errs = append(errs, s.err)
			continue
		}
		succeeded++
		if s.text != "" {
			candidates = append(candidates, s.text)
		}
	}
	if succeeded == 0 {
		return "", nil, fmt.Errorf("transcribe: every STT engine failed: %w", errors.Join(errs...))
	}
	if len(candidates) == 0 {
		return "", nil, nil // every engine heard silence — not an error
	}
	if len(candidates) == 1 {
		return candidates[0], candidates, nil
	}

	text, serr := p.synthesizeTranscript(ctx, p.LLM, p.ChatModel, summary, recent, candidates)
	if serr != nil {
		log.Printf("transcribe: reconcile: %v; falling back to first candidate", serr)
		return candidates[0], candidates, nil
	}
	if text = strings.TrimSpace(text); text == "" {
		return candidates[0], candidates, nil
	}
	return text, candidates, nil
}

// synthesizeTranscript asks an LLM to reconcile one or more STT engines'
// independent (and possibly disagreeing) transcriptions of the same
// utterance into the single sentence the learner most likely said, using the
// conversation as it stood before this turn — STT ambiguity (homophones,
// mis-heard words) is often only resolvable with that context. It must NOT
// correct grammar or rewrite the sentence into "proper" English: the
// learner's actual mistakes have to reach correct() and the LLM chat context
// unchanged (see session package doc) — this only disambiguates what was
// heard, never rewrites what should have been said.
func (p *Pipeline) synthesizeTranscript(ctx context.Context, client llm.Client, model string, summary string, recent []llm.Message, candidates []string) (string, error) {
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: transcriptSynthesisSystemPrompt},
		{Role: llm.RoleUser, Content: renderTranscriptSynthesisInput(summary, recent, candidates)},
	}
	return client.Complete(ctx, model, msgs, false)
}

const transcriptSynthesisSystemPrompt = `You are a transcription-reconciliation assistant for a spoken English
conversation. Independent speech-to-text engines transcribed the SAME short
utterance from a language learner; they may disagree, especially on words
that sound alike. Using the conversation so far for context, decide the
single sentence the learner most likely actually said.
Rules:
- Return ONLY that sentence. No labels, no quotes, no alternatives, no commentary.
- If every candidate already agrees, return it unchanged.
- Preserve the learner's actual words and grammar EXACTLY as transcribed,
  mistakes included — resolve disagreements between the candidates, do not
  correct grammar or rewrite the sentence into "proper" English.`

func renderTranscriptSynthesisInput(summary string, recent []llm.Message, candidates []string) string {
	var b strings.Builder
	if summary != "" {
		b.WriteString("Long-term memory of this learner:\n" + summary + "\n\n")
	}
	if len(recent) > 0 {
		b.WriteString("Conversation so far:\n")
		writeTranscript(&b, recent)
		b.WriteString("\n")
	}
	b.WriteString("Candidate transcriptions of the learner's next line:\n")
	for i, c := range candidates {
		fmt.Fprintf(&b, "%d. %s\n", i+1, c)
	}
	return b.String()
}
