// Package session holds one connection's conversation state: a short
// verbatim window of recent turns plus a compact long-term summary. The
// verbatim window keeps the learner's actual words (mistakes included) so the
// LLM sees real input — grammar correction is shown as separate feedback,
// never silently rewritten into what the LLM sees. Once the window grows past
// pipeline.MaxHistoryMessages, the oldest turns are folded into the summary
// (see PeekOldestForCompaction/ApplyCompaction) so long conversations stay
// cheap to store (see internal/store) and to send to the LLM.
package session

import (
	"sync"

	"buddy/server/internal/llm"
)

type Session struct {
	mu      sync.Mutex
	system  string
	summary string
	history []llm.Message // verbatim user/assistant turns, most-recent window only
	turn    int
}

func New(systemPrompt string) *Session {
	return &Session{system: systemPrompt}
}

// Seed restores long-term memory (loaded from internal/store) into a fresh
// session, e.g. right after a client connects. lastTurn is the highest turn
// number already persisted for this session (0 for a brand-new one, see
// store.Store.LastTurn) — without it, NextTurn would restart numbering at 1
// on every reconnect and collide with, silently overwriting, turns already
// saved under those same numbers.
func (s *Session) Seed(summary string, recent []llm.Message, lastTurn int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.summary = summary
	s.history = append([]llm.Message(nil), recent...)
	s.turn = lastTurn
}

// Export returns the current summary and verbatim window for persistence.
func (s *Session) Export() (summary string, recent []llm.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.summary, append([]llm.Message(nil), s.history...)
}

// NextTurn increments and returns the current turn id.
func (s *Session) NextTurn() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turn++
	return s.turn
}

func (s *Session) AppendUser(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = append(s.history, llm.Message{Role: llm.RoleUser, Content: text})
}

func (s *Session) AppendAssistant(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = append(s.history, llm.Message{Role: llm.RoleAssistant, Content: text})
}

// ReplaceLastUser upgrades the last user message to the high-quality
// transcription produced by the refine track, keeping the conversation
// context accurate for subsequent turns.
func (s *Session) ReplaceLastUser(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.history) - 1; i >= 0; i-- {
		if s.history[i].Role == llm.RoleUser {
			s.history[i].Content = text
			return
		}
	}
}

// Snapshot returns the full message list for a stateless LLM call: system
// prompt, long-term summary (if any), then the verbatim recent window.
func (s *Session) Snapshot() []llm.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]llm.Message, 0, len(s.history)+2)
	out = append(out, llm.Message{Role: llm.RoleSystem, Content: s.system})
	if s.summary != "" {
		out = append(out, llm.Message{
			Role:    llm.RoleSystem,
			Content: "Long-term memory of this learner (compressed, for context only):\n" + s.summary,
		})
	}
	out = append(out, s.history...)
	return out
}

// PeekOldestForCompaction returns the oldest verbatim messages to fold into
// the summary, without mutating state, once the window exceeds max (ok=false
// otherwise). The caller runs the LLM summarization outside the lock, then
// commits with ApplyCompaction — so a failed LLM call never loses history.
func (s *Session) PeekOldestForCompaction(max int) (old []llm.Message, curSummary string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if max <= 0 || len(s.history) <= max {
		return nil, "", false
	}
	drop := len(s.history) - max/2 // fold down to half of max, leaving room to grow again
	return append([]llm.Message(nil), s.history[:drop]...), s.summary, true
}

// ApplyCompaction commits a newly rolled-up summary and drops the n oldest
// verbatim messages that were folded into it.
func (s *Session) ApplyCompaction(newSummary string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n > len(s.history) {
		n = len(s.history)
	}
	s.summary = newSummary
	s.history = append([]llm.Message(nil), s.history[n:]...)
}
