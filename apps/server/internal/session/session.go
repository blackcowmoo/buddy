// Package session holds per-connection conversation state. It is the shared
// context that the "middle LLM" keeps clean: the refine track can rewrite the
// last user turn in place once the high-quality transcription lands.
package session

import (
	"sync"

	"buddy/server/internal/llm"
)

type Session struct {
	mu      sync.Mutex
	history []llm.Message
	turn    int
}

func New(systemPrompt string) *Session {
	return &Session{
		history: []llm.Message{{Role: llm.RoleSystem, Content: systemPrompt}},
	}
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

// Snapshot returns a copy of the history for a stateless LLM call.
func (s *Session) Snapshot() []llm.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]llm.Message, len(s.history))
	copy(out, s.history)
	return out
}

func (s *Session) Reset(systemPrompt string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = []llm.Message{{Role: llm.RoleSystem, Content: systemPrompt}}
	s.turn = 0
}
