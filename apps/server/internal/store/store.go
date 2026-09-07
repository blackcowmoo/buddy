// Package store persists each learner's conversations. A user can have many
// sessions (chat rooms, in the frontend's terms); each session carries its
// own compacting long-term memory (Profile, see internal/session) plus a
// full, uncompacted transcript (Turn) used to replay the session and review
// past corrections. Everything persisted here is text — audio is never
// written to the store; it lives only in memory for the duration of one STT
// call (see internal/stt) and is discarded immediately after.
package store

import (
	"context"
	"encoding/json"
	"errors"

	"buddy/server/internal/llm"
	"buddy/server/internal/protocol"
)

// ErrNotFound is returned by SessionDetail when no session matches — either
// it doesn't exist, or it belongs to a different user. The two cases are
// deliberately indistinguishable to the caller, so an API response can't be
// used to probe for the existence of someone else's session.
var ErrNotFound = errors.New("store: not found")

// Profile is one session's persistent LLM-context memory: a compact running
// summary plus a short verbatim window (see internal/session, which compacts
// the window down to this shape as it grows).
type Profile struct {
	Summary string
	Recent  []llm.Message
}

// SessionMeta describes one chat room for listing/display.
type SessionMeta struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
	// Ended is true once the learner has confirmed "end this conversation"
	// (see EndSession) — the room is permanently read-only from then on.
	// EndSession freezes it immediately; StudySummary/StudySummaryStatus
	// below fill in afterward, once the background wrap-up job finishes.
	Ended bool `json:"ended"`
	// StudySummary is the wrap-up saved by CompleteStudySummary — nil until
	// StudySummaryStatus reaches JobStatusDone. Each sentence is written in
	// English with a paired native-language translation (see
	// protocol.StudySummarySentence and pipeline.GenerateStudySummary).
	StudySummary []protocol.StudySummarySentence `json:"studySummary,omitempty"`
	// StudySummaryStatus is the end-of-conversation wrap-up job's status
	// (JobStatusPending/JobStatusDone/JobStatusFailed), set to
	// JobStatusPending the instant EndSession freezes the room and updated
	// by whichever replica's asyncjob.KindStudySummary worker actually runs
	// it (see transport.StudySummaryJobHandler) — independent of the
	// connection that confirmed "end this conversation", so it keeps
	// progressing (and this field keeps reflecting that) even if the
	// learner has already navigated away. "" for a session that hasn't been
	// ended, or one ended before this became an async job — its
	// StudySummary was already generated synchronously, so there's nothing
	// left to poll for (see EndConversationControl in apps/web/src/App.tsx,
	// which treats "" the same as JobStatusDone for that reason).
	StudySummaryStatus string `json:"studySummaryStatus,omitempty"`
	// Quiz is the pre-generated practice quiz saved by CompleteStudyQuiz —
	// nil until QuizStatus reaches JobStatusDone. Generated alongside
	// StudySummary, from the same flagged issues, right when EndSession
	// freezes the room (see asyncjob.KindStudyQuiz), so opening the quiz
	// (the "퀴즈 풀기" button) reads an already-finished result instead of
	// waiting on an LLM call in the request path.
	Quiz []protocol.QuizQuestion `json:"quiz,omitempty"`
	// QuizStatus mirrors StudySummaryStatus but for asyncjob.KindStudyQuiz —
	// a separate column (not reused from StudySummaryStatus) because the two
	// jobs run independently, in parallel, not one after the other. "" means
	// no quiz job has ever run for this session (e.g. one ended before this
	// feature existed) — see needsStudyQuizBackfill, which re-triggers
	// exactly that state the next time the session is viewed.
	QuizStatus string `json:"quizStatus,omitempty"`
	// QuizCompleted is a one-way "studied this" checkmark for the room list
	// (see ListSessions): set by MarkQuizCompleted once the learner either
	// finishes every quiz question — right or wrong, this isn't a "got it
	// right" flag — or, for a session with no quiz-worthy issues (Quiz
	// empty), acknowledges it via the "내가 읽었음" button instead. Never
	// cleared once set (the frontend hides the "퀴즈 풀기" button once this is
	// true, offering "퀴즈 다시 만들기" — a fresh question set — instead of
	// letting the same quiz be retaken).
	QuizCompleted bool `json:"quizCompleted,omitempty"`
	// Instant is true for a room flagged via MarkInstant (an "오늘의 한 문장"
	// one-turn conversation). Populated by SessionDetail so
	// transport.CorrectionJobHandler can tell, right when a turn's grammar
	// correction lands, whether this session should auto-finalize itself
	// (see maybeFinalizeInstantSession) instead of waiting for the learner
	// to press the manual 종료 button.
	Instant bool `json:"instant,omitempty"`
	// UnreadCorrections counts final Judge feedback that differs from the
	// quick Chat preview and has not yet been opened by the learner. Kept on
	// each room row (rather than as a global notification) so the badge always
	// points at the exact conversation containing the updated feedback.
	UnreadCorrections int `json:"unreadCorrections,omitempty"`
}

// Turn is one persisted message in a session's full transcript — the source
// of truth for replaying a session and reviewing past corrections. Unlike
// Profile.Recent, this is never compacted or dropped as the conversation
// grows.
type Turn struct {
	Turn    int    `json:"turn"`
	Role    string `json:"role"` // "user" | "assistant"
	Text    string `json:"text"`
	Refined bool   `json:"refined"`
	// CreatedAt is when this turn was first saved (unix seconds) — set once,
	// at insert, and never touched by a later upsert (e.g. refined_transcript
	// overwriting text). Lets the frontend group a replayed transcript into
	// date-divided days and show a per-message time.
	CreatedAt int64 `json:"createdAt"`
	// Source is how the learner produced this turn — protocol.SourceVoice or
	// protocol.SourceText. Empty for assistant turns, which are always
	// generated rather than input by the learner.
	Source     string               `json:"source,omitempty"`
	Correction *protocol.Correction `json:"correction,omitempty"`
	// Translation is a native-language translation of Text, set for both
	// user and assistant turns (see SaveTranslation). Kept separate from
	// Correction, which is user-only and scoped to grammar feedback.
	Translation string `json:"translation,omitempty"`
	// Meta is an open-ended bag for signals beyond the transcript itself —
	// e.g. a future emotion/tone classifier's output. Nothing populates it
	// yet; it exists so that lands as a data addition, not another schema
	// migration. Opaque on purpose: store doesn't know or care what keys it
	// contains.
	Meta json.RawMessage `json:"meta,omitempty"`
	// ReplyStatus is this turn's reply-job status ("pending"/"processing"/
	// "done"/"failed"), populated only for an assistant turn reserved via
	// ReserveAssistantTurn. Empty for a user turn, and for an assistant turn
	// saved before this feature existed (SaveTurn, not ReserveAssistantTurn
	// + CompleteAssistantTurn) — both cases mean "nothing to poll for", so
	// the frontend doesn't need to tell them apart.
	ReplyStatus string `json:"replyStatus,omitempty"`
	// CorrectionStatus mirrors ReplyStatus for the grammar-correction job on
	// a user turn, populated only when it was reserved via
	// ReserveCorrectionJob. In particular JobStatusFailed here — as opposed
	// to Correction present with an empty Issues slice — is what lets the
	// frontend tell "the analysis pass ran and found nothing to flag" apart
	// from "the analysis pass itself errored", instead of both looking like
	// a turn that just never got a correction (see GrammarControl in
	// apps/web/src/App.tsx). Empty for an assistant turn, and for a user
	// turn saved before this feature existed.
	CorrectionStatus string `json:"correctionStatus,omitempty"`
	// CorrectionStage is "chat" while Correction is the quick preview and
	// "judge" after the terminal cascade result has landed. CorrectionUnread
	// is server-authoritative and is cleared only by MarkCorrectionRead.
	CorrectionStage  string `json:"correctionStage,omitempty"`
	CorrectionUnread bool   `json:"correctionUnread,omitempty"`
}

// Job status values for the (user_id, session_id, turn, kind) rows tracked
// alongside a turn's content — see ReserveAssistantTurn/CompleteAssistantTurn/
// FailJob/JobStatus, and internal/asyncjob for the queue that drives a job
// through these states.
const (
	JobStatusPending = "pending"
	JobStatusDone    = "done"
	JobStatusFailed  = "failed"
)

// Store persists everything keyed by an opaque user ID (see
// internal/identity) and, within a user, an opaque session ID (one per chat
// room). Every method takes both IDs together so one user's data is never
// reachable through another user's request, even if a session ID leaks or is
// guessed — see the composite primary keys in internal/store/mysql.go.
type Store interface {
	// Load returns the zero Profile (not an error) if the session has no
	// record yet — e.g. a brand-new session ID, or one that belongs to a
	// different user (never surfaced as such; see ErrNotFound).
	Load(ctx context.Context, userID, sessionID string) (Profile, error)
	Save(ctx context.Context, userID, sessionID string, p Profile) error

	// GetInterlocutorStyle returns userID's saved free-text preference for
	// how the AI conversation partner should talk to them (e.g. "ask
	// interview-style questions", "sound like a professional") — "" if never
	// set. It's global to the user, not scoped to one session: see
	// pipeline.BuildSystemPrompt, which layers it onto the chat persona when
	// a session is created (transport.Handler.ServeHTTP).
	GetInterlocutorStyle(ctx context.Context, userID string) (string, error)
	// SaveInterlocutorStyle persists userID's conversation-style preference,
	// replacing any previous value. An empty style clears it back to the
	// default persona.
	SaveInterlocutorStyle(ctx context.Context, userID, style string) error

	// SaveTurn upserts one message into a session's transcript, creating the
	// session's row (and its title, derived from turn 1's text) on first
	// write. source is protocol.SourceVoice/SourceText for a user turn, or ""
	// for an assistant turn.
	SaveTurn(ctx context.Context, userID, sessionID string, turn int, role, text string, refined bool, source string) error
	// SaveCorrection attaches grammar/vocabulary feedback to an existing
	// user turn and, if a correction job was reserved for it (see
	// ReserveCorrectionJob), marks that job done in the same transaction —
	// so a poller never observes a "done" status before the correction text
	// it belongs to is actually visible. A no-op on the turn write if that
	// turn hasn't been saved yet; a no-op on the job write if none was ever
	// reserved (e.g. a caller that predates ReserveCorrectionJob or a test
	// double).
	SaveCorrection(ctx context.Context, userID, sessionID string, turn int, c protocol.Correction) error
	// SaveCorrectionFinal is the stage-aware form used by live/queued
	// pipelines. unread comes from comparing the actual Chat and Judge outputs,
	// not from racing a preview write against this terminal write.
	SaveCorrectionFinal(ctx context.Context, userID, sessionID string, turn int, c protocol.Correction, unread bool) error
	// SaveCorrectionPreview persists the immediately-visible Chat result
	// without marking the correction job done. A late preview never overwrites
	// an already-saved Judge result.
	SaveCorrectionPreview(ctx context.Context, userID, sessionID string, turn int, c protocol.Correction) error
	// MarkCorrectionRead clears the unread marker for one final correction.
	// Scoped by both user and session so another learner's turn can never be
	// acknowledged by a guessed ID.
	MarkCorrectionRead(ctx context.Context, userID, sessionID string, turn int) error
	// ReserveCorrectionJob writes a "pending" correction-job row for
	// (userID, sessionID, turn), before that turn's correction job is even
	// enqueued — mirrors ReserveAssistantTurn, minus the placeholder-text
	// insert (the user turn this job corrects has already been saved by the
	// time correction ever runs). A no-op if this exact job was already
	// reserved (e.g. a race between two connections, or a retry).
	ReserveCorrectionJob(ctx context.Context, userID, sessionID string, turn int) error
	// SaveTranslation attaches a native-language translation to an existing
	// turn. role disambiguates a user turn from its paired assistant turn,
	// since both share the same turn number. A no-op if that turn hasn't
	// been saved yet.
	SaveTranslation(ctx context.Context, userID, sessionID string, turn int, role, translation string) error

	// ReserveAssistantTurn writes a placeholder assistant-turn row (empty
	// text) plus a "pending" reply-job row for (userID, sessionID, turn),
	// before that turn's reply job is even enqueued — so a poller (see
	// JobStatus, and Turn.ReplyStatus in SessionDetail) has something to
	// observe immediately, and durably records that this turn's reply is
	// in flight even if the enqueuing process dies before the job runs. A
	// no-op if this exact reply job was already reserved (e.g. a race
	// between two connections, or a retry) — it never clobbers an
	// already-completed row's text.
	ReserveAssistantTurn(ctx context.Context, userID, sessionID string, turn int) error
	// CompleteAssistantTurn writes the finished assistant reply text and
	// marks (userID, sessionID, turn)'s reply job "done", atomically. Called
	// by whichever replica's worker actually ran the reply job — see
	// internal/pipeline.ReplyJobHandler — regardless of whether that's the
	// same replica the learner's WebSocket connection is still on.
	CompleteAssistantTurn(ctx context.Context, userID, sessionID string, turn int, text string) error
	// FailJob marks (userID, sessionID, turn, kind)'s job "failed" with
	// errMsg, for observability. This does not itself stop retries —
	// internal/asyncjob's stale-claim reaper retries a job regardless of
	// this status — it only records the most recent attempt's outcome.
	FailJob(ctx context.Context, userID, sessionID string, turn int, kind, errMsg string) error
	// JobStatus returns (userID, sessionID, turn, kind)'s current status
	// (JobStatusPending/"processing"/JobStatusDone/JobStatusFailed), or ""
	// if no such job was ever reserved (e.g. a turn from before "reply" or
	// "correction" job tracking existed, or a kind that isn't tracked this
	// way at all).
	JobStatus(ctx context.Context, userID, sessionID string, turn int, kind string) (string, error)
	// AssistantTurnText returns just one turn's assistant reply text ("" if
	// that turn has no assistant row yet). A narrow single-row read for the
	// reply-poll fallback path (see internal/transport's
	// pollReplyUntilDone), which only needs the one text a finished reply
	// job wrote — SessionDetail would fetch the session row plus the entire
	// transcript, with its per-turn job-status joins, to answer the same
	// question.
	AssistantTurnText(ctx context.Context, userID, sessionID string, turn int) (string, error)

	// LastTurn returns the highest turn number already persisted for a
	// session (0 if none), so a resumed session can continue numbering
	// turns from where the transcript left off instead of restarting at 0
	// and colliding with — silently overwriting — turns already saved
	// under those same numbers. See internal/session.Session.Seed.
	LastTurn(ctx context.Context, userID, sessionID string) (int, error)

	// SaveGeneratedTitle sets a session's title to an LLM-generated one,
	// exactly once — a no-op if this session's title was already
	// auto-generated (see internal/transport, which triggers this once per
	// WS connection's first turn; a reconnect resets its own turn counter,
	// so this guard is what actually keeps the title from being
	// regenerated and flapping on every reconnect). Also a no-op if the
	// session row doesn't exist yet.
	SaveGeneratedTitle(ctx context.Context, userID, sessionID, title string) error

	// EndSession permanently marks a session read-only: SessionMeta.Ended
	// becomes true and StudySummaryStatus becomes JobStatusPending, both
	// immediately — freezing the room never waits on the wrap-up LLM call
	// (see asyncjob.KindStudySummary, enqueued right after this by
	// httpserver.sessionEndHandler so the room stays frozen, and its
	// summary keeps generating, even if the learner's connection is already
	// gone). CompleteStudySummary/FailStudySummary fill in the actual
	// outcome once that background job finishes. A no-op (nil error) if
	// sessionID doesn't exist or belongs to a different user, same as Save.
	EndSession(ctx context.Context, userID, sessionID string) error
	// SessionEnded reports whether a session is already frozen (see
	// EndSession) — false, nil if sessionID doesn't exist or belongs to a
	// different user. A narrow single-column read for transport's WS read
	// loop, called on every incoming message to skip dispatching a reply/
	// correction for a room that's already read-only (e.g. maybeFinalizeInstantSession
	// closed it out from under a still-open connection) — same
	// over-fetch-avoidance reasoning as AssistantTurnText: SessionDetail
	// would pull the whole transcript to answer this one boolean.
	SessionEnded(ctx context.Context, userID, sessionID string) (bool, error)
	// CompleteStudySummary saves the wrap-up an asyncjob.KindStudySummary
	// job generated for an already-ended session — StudySummary becomes
	// summary and StudySummaryStatus becomes JobStatusDone, the terminal
	// state a reopened room (or the room list) polls for. A no-op if
	// sessionID doesn't exist or belongs to a different user.
	CompleteStudySummary(ctx context.Context, userID, sessionID string, summary []protocol.StudySummarySentence) error
	// FailStudySummary marks StudySummaryStatus JobStatusFailed after an
	// asyncjob.KindStudySummary job's LLM call errored — the reaper still
	// retries the job from scratch regardless (see asyncjob.Queue.Execute),
	// this only records the most recent attempt's outcome for a poller in
	// the meantime, same reasoning as FailJob.
	FailStudySummary(ctx context.Context, userID, sessionID string) error
	// RestartStudySummary resets an already-terminal (JobStatusDone or
	// JobStatusFailed) wrap-up back to JobStatusPending and clears any stored
	// StudySummary, so asyncjob.KindStudySummary regenerates it from scratch
	// — the manual counterpart to needsStudySummaryBackfill's automatic
	// retrigger, for exactly the case that one doesn't cover: a session that
	// landed as JobStatusDone with an empty summary (see
	// httpserver.sessionRestudyHandler, the "다시 확인하기" button in
	// EndConversationControl) rather than one still JobStatusPending. A
	// no-op if sessionID doesn't exist or belongs to a different user, same
	// as Save.
	RestartStudySummary(ctx context.Context, userID, sessionID string) error

	// CompleteStudyQuiz saves the quiz an asyncjob.KindStudyQuiz job
	// generated for an already-ended session — Quiz becomes questions and
	// QuizStatus becomes JobStatusDone. Mirrors CompleteStudySummary; a
	// no-op if sessionID doesn't exist or belongs to a different user.
	CompleteStudyQuiz(ctx context.Context, userID, sessionID string, questions []protocol.QuizQuestion) error
	// FailStudyQuiz marks QuizStatus JobStatusFailed after an
	// asyncjob.KindStudyQuiz job's LLM call errored — mirrors
	// FailStudySummary; the reaper still retries the job from scratch
	// regardless (see asyncjob.Queue.Execute).
	FailStudyQuiz(ctx context.Context, userID, sessionID string) error
	// MarkQuizCompleted sets QuizCompleted true — see its doc comment on
	// SessionMeta for when this is called and why it never gets cleared
	// again. A no-op if sessionID doesn't exist or belongs to a different
	// user, same as Save.
	MarkQuizCompleted(ctx context.Context, userID, sessionID string) error
	// RestartStudyQuiz resets an already-terminal (JobStatusDone or
	// JobStatusFailed) quiz back to JobStatusPending, clears any stored Quiz,
	// and clears QuizCompleted — so asyncjob.KindStudyQuiz regenerates a
	// fresh set of questions from scratch. Unlike RestartStudySummary (only
	// ever called to recover a stuck empty result), this is meant to be
	// called on a quiz that already has real content too: the "퀴즈 다시
	// 만들기" button (see httpserver.sessionQuizResetHandler) lets a learner
	// ask for a brand-new quiz on demand, e.g. one generated before
	// AnswerMeaning/AcceptableAnswers existed. Clearing QuizCompleted matters
	// here specifically — the old quiz's "studied this" checkmark must not
	// carry over to questions the learner hasn't actually seen yet. A no-op
	// if sessionID doesn't exist or belongs to a different user, same as
	// Save.
	RestartStudyQuiz(ctx context.Context, userID, sessionID string) error

	// GetLearnerProfile returns userID's persistent, LLM-maintained
	// cross-session profile (recurring mistakes, interests, proficiency
	// trend) — "" if never set. Unlike Profile (per-session, folded from
	// verbatim turns), this is folded from each session's own StudySummary
	// as it ends (see pipeline.UpdateLearnerProfile) and carries forward
	// into every session's system prompt (pipeline.BuildSystemPrompt),
	// including ones in a different chat room entirely.
	GetLearnerProfile(ctx context.Context, userID string) (string, error)
	// SaveLearnerProfile persists userID's cross-session profile, replacing
	// any previous value.
	SaveLearnerProfile(ctx context.Context, userID, profile string) error

	// GetWordAutoAddStatus returns userID's current "새 단어 추가로 학습하기"
	// job status (JobStatusPending/JobStatusDone/JobStatusFailed) and, once
	// JobStatusDone, how many words that run actually added — see
	// StartWordAutoAdd/CompleteWordAutoAdd. "" means no run has ever started
	// (or MySQL's column default for a settings row created before this
	// feature existed) — httpserver.wordAutoAddHandler and its frontend poll
	// treat that the same as an idle, not-in-progress state.
	GetWordAutoAddStatus(ctx context.Context, userID string) (status string, addedCount int, err error)
	// StartWordAutoAdd marks userID's auto-add job JobStatusPending and
	// resets addedCount to 0, right when "새 단어 추가로 학습하기" is pressed —
	// httpserver.wordAutoAddHandler returns as soon as this call lands,
	// before transport.EnqueueWordAutoAddJob's SuggestNewWords call even
	// starts, so the request survives the learner navigating away (see
	// asyncjob.KindWordAutoAdd).
	StartWordAutoAdd(ctx context.Context, userID string) error
	// CompleteWordAutoAdd marks userID's auto-add job JobStatusDone with how
	// many words it actually added (0 if the model suggested nothing new, or
	// everything it suggested failed validation) — see
	// transport.runWordAutoAdd.
	CompleteWordAutoAdd(ctx context.Context, userID string, addedCount int) error
	// FailWordAutoAdd marks userID's auto-add job JobStatusFailed after
	// pipeline.Pipeline.SuggestNewWords itself errored (an individual
	// suggestion failing validation is just skipped, not a failure of the
	// whole run — see transport.runWordAutoAdd). Terminal for this run: the
	// learner has to press the button again to retry, same as
	// newsarticle.Article's StatusFailed.
	FailWordAutoAdd(ctx context.Context, userID string) error

	// ListSessions returns userID's chat rooms, most recently active first.
	// Only sessions with at least one saved turn appear (see SaveTurn).
	// Instant/"오늘의 한 문장" rooms (see MarkInstant) are excluded — they have
	// their own separate list, ListInstantSessions.
	ListSessions(ctx context.Context, userID string) ([]SessionMeta, error)
	// ListInstantSessions returns userID's instant/"오늘의 한 문장" rooms, most
	// recently active first — the mirror image of ListSessions' exclusion.
	ListInstantSessions(ctx context.Context, userID string) ([]SessionMeta, error)
	// MarkInstant flags a session as an instant/"오늘의 한 문장" conversation
	// (see SessionMeta's doc and ListInstantSessions above), creating its row
	// if this races SaveTurn's own row-creating write for the same brand-new
	// session. A no-op in effect if sessionID doesn't exist yet under a
	// different user — the row it creates is always scoped to userID.
	MarkInstant(ctx context.Context, userID, sessionID string) error
	// ListSessionsWithStudySummary returns every one of userID's ended
	// sessions that folded a non-empty study summary into the learner
	// profile, oldest-ended first — see MySQLStore.ListSessionsWithStudySummary
	// for why that order matters (replaying a from-scratch profile rebuild
	// after a contributing session is deleted — see
	// transport.runProfileRegenerate).
	ListSessionsWithStudySummary(ctx context.Context, userID string) ([]SessionMeta, error)
	// SessionDetail returns one session's full transcript, in turn order.
	// Returns ErrNotFound if it doesn't exist or belongs to a different user.
	// Callers that need to reason about the whole session — translation
	// backfill, reply-status polling, LLM context hydration — want this, not
	// SessionDetailPage.
	SessionDetail(ctx context.Context, userID, sessionID string) (SessionMeta, []Turn, error)

	// SessionDetailPage returns one page of a session's transcript, in turn
	// order, for UI display: at most `limit` most recent distinct turns with
	// turn < beforeTurn (or the most recent `limit` turns overall when
	// beforeTurn <= 0), and the bool return reports whether older turns
	// still exist beyond the page — the frontend's cue to fetch another page
	// on scrolling up rather than assuming it has the whole history.
	// Returns ErrNotFound if the session doesn't exist or belongs to a
	// different user.
	SessionDetailPage(ctx context.Context, userID, sessionID string, beforeTurn, limit int) (SessionMeta, []Turn, bool, error)

	// DeleteSession removes a session and its full transcript. A no-op (nil
	// error) if sessionID doesn't exist or belongs to a different user — same
	// indistinguishable-from-missing contract as Load.
	DeleteSession(ctx context.Context, userID, sessionID string) error

	Close() error
}
