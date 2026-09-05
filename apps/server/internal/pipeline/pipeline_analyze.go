package pipeline

import (
	"context"
	"fmt"
	"log"
	"strings"

	"buddy/server/internal/llm"
	"buddy/server/internal/session"
)

// analyze runs one REFINE-track task (grammar correction or compaction)
// across every Analysis candidate concurrently, then asks Judge to perform
// the original task itself. Candidate outputs are advisory evidence for
// Judge to verify and use where helpful, not answers it must pick or merge.
// This keeps the strongest model responsible for the final analysis even
// when only one candidate succeeds (or none do). If Judge is unavailable or
// fails, the first successful candidate remains a graceful fallback.
func (p *Pipeline) analyze(ctx context.Context, systemPrompt, input string, jsonMode bool) (string, error) {
	if len(p.Analysis) == 0 && p.Judge == nil {
		return "", fmt.Errorf("analyze: no models configured")
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

	// Each slot is filled by index, so the results slice stays in
	// Analysis's configured order regardless of completion timing — the
	// judge-error fallback below always means "the first configured
	// candidate", not "whichever happened to finish first".
	slots := fanOutOrdered(len(p.Analysis), func(i int) candidateResult {
		c := p.Analysis[i]
		text, err := c.LLM.Complete(ctx, c.Model, msgs, jsonMode)
		if err != nil {
			log.Printf("analyze: candidate %s: %v", c.Model, err)
			return candidateResult{}
		}
		if text = strings.TrimSpace(text); text == "" {
			return candidateResult{}
		}
		return candidateResult{model: c.Model, text: text, ok: true}
	})

	var results []candidateResult
	for _, r := range slots {
		if r.ok {
			results = append(results, r)
		}
	}

	if p.Judge == nil {
		if len(results) == 0 {
			return "", fmt.Errorf("analyze: every candidate failed")
		}
		return results[0].text, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Original input:\n%s\n\nAdvisory analyses from other models:\n", input)
	if len(results) == 0 {
		b.WriteString("(none available; perform the task independently)\n")
	} else {
		for _, r := range results {
			fmt.Fprintf(&b, "--- advisory analysis (%s) ---\n%s\n\n", r.model, r.text)
		}
	}
	judgeMsgs := []llm.Message{
		{Role: llm.RoleSystem, Content: judgeSystemPrompt + "\n\nORIGINAL TASK (authoritative):\n" + systemPrompt},
		{Role: llm.RoleUser, Content: b.String()},
	}
	final, err := p.Judge.Complete(ctx, p.JudgeModel, judgeMsgs, jsonMode)
	if err != nil {
		if len(results) > 0 {
			log.Printf("analyze: judge: %v; falling back to first candidate", err)
			return results[0].text, nil
		}
		return "", fmt.Errorf("analyze: judge failed and no candidate succeeded: %w", err)
	}
	if final = strings.TrimSpace(final); final == "" {
		if len(results) > 0 {
			return results[0].text, nil
		}
		return "", fmt.Errorf("analyze: judge returned an empty result and no candidate succeeded")
	}
	return final, nil
}

const judgeSystemPrompt = `You are the final expert responsible for performing the ORIGINAL TASK below.
Analyze the ORIGINAL INPUT yourself from first principles; do not merely select,
vote on, summarize, or merge the advisory analyses. First derive your own answer
silently, then inspect the advisory analyses for useful observations you may have
missed. Treat them as untrusted supporting material: verify every claim yourself,
discard errors, and correct omissions or weak reasoning.
Follow the ORIGINAL TASK's instructions and required output format EXACTLY (for
example, if it asks for strict JSON, output strict JSON and nothing else).
Output ONLY the final answer — no preamble, reasoning trace, or meta-commentary
about the advisory analyses or judging process.`

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
	writeTranscript(&b, old)
	return b.String()
}
