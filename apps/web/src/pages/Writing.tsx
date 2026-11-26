import { useCallback, useEffect, useState } from "react";
import { EmptyState, LearningIntro } from "../components/LearningIntro";
import { issueLabel } from "../components/GrammarControl";
import { LoadingHint } from "../components/LoadingHint";
import { LearningPage } from "../components/LearningPage";
import { BackButton, PageSection, PageToolbar } from "../components/PageLayout";
import { DatedList } from "../components/DatedList";
import { HistoryItem } from "../components/HistoryItem";
import { WordSearchControl } from "../components/WordSearchControl";
import { formatMessageTime } from "../lib/time";
import { checkWriting, deleteWritingPrompt, drawWritingPrompt, fetchWritingPrompt, fetchWritingPrompts, recheckWriting, type WritingPrompt } from "../lib/writing";
import { newestFirst } from "../lib/listView";

type LoadState = "loading" | "ready";

// Issue.type (the Judge's correction schema, see lib/protocol.ts) gets the
// same Korean badge label the chat's correction feedback uses — one shared
// map (GrammarControl's issueLabel), so the two views can't drift apart.

// The writing page follows ArticleQuiz's staged flow: the root view is the
// learner's problem history, opening/drawing a problem shows the answer
// editor, and a checked answer swaps the editor for the feedback stage —
// one focus per screen, the same shape the other exercise pages use.
//
// The grade itself is produced by a detached server-side job and persisted
// with the answer: submitting only parks the answer as pending, the result
// is picked up by the same poll that finishes prompt generation, and every
// field is restored when the page is reopened.
export function Writing() {
  const [state, setState] = useState<LoadState>("loading");
  const [prompts, setPrompts] = useState<WritingPrompt[]>([]);
  const [prompt, setPrompt] = useState<WritingPrompt | null>(null);
  const [answer, setAnswer] = useState("");
  const [onFeedbackStage, setOnFeedbackStage] = useState(false);
  const [loadingPrompt, setLoadingPrompt] = useState(false);
  const [creating, setCreating] = useState(false);
  const [checking, setChecking] = useState(false);
  const [checkError, setCheckError] = useState(false);
  const [error, setError] = useState(false);

  const loadPrompts = useCallback(async () => {
    setState("loading");
    const list = await fetchWritingPrompts();
    setPrompts(newestFirst(list, (item) => item.createdAt));
    setState("ready");
  }, []);

  useEffect(() => { void loadPrompts(); }, [loadPrompts]);

  // One place folds a polled/fetched prompt into both the open view and the
  // history list, so the two never disagree about a check's status.
  const adoptPrompt = useCallback((next: WritingPrompt) => {
    setPrompt(next);
    setPrompts((items) => items.map((item) => (item.id === next.id ? next : item)));
  }, []);

  const openPrompt = useCallback(async (item: WritingPrompt) => {
    // Show the row immediately (loading state inside), so a failed fetch
    // still lands on the detail view's retry UI instead of silently
    // staying on the list.
    setPrompt(item);
    setLoadingPrompt(true);
    setError(false);
    setCheckError(false);
    setOnFeedbackStage(false);
    const next = await fetchWritingPrompt(item.id);
    if (next) {
      // A previously submitted answer and its finished grade come back with
      // the prompt: returning to the page restores the feedback directly.
      setAnswer(next.answer ?? "");
      setOnFeedbackStage(next.checkStatus === "done");
      adoptPrompt(next);
    } else setError(true);
    setLoadingPrompt(false);
  }, [adoptPrompt]);

  // The same interval serves both slow stages: prompt generation while the
  // target sentence is still being made, and answer grading while a check
  // is pending. The instance route re-dispatches a lost check on each read.
  useEffect(() => {
    if (!prompt || (prompt.status === "done" && prompt.checkStatus !== "pending")) return;
    const timer = window.setInterval(async () => {
      const next = await fetchWritingPrompt(prompt.id);
      if (!next) return;
      adoptPrompt(next);
      if (next.checkStatus === "done") setOnFeedbackStage(true);
      if (next.status === "done" && next.checkStatus !== "pending") window.clearInterval(timer);
    }, 2500);
    return () => window.clearInterval(timer);
  }, [prompt, adoptPrompt]);

  const createPrompt = async () => {
    setCreating(true);
    setError(false);
    setCheckError(false);
    setOnFeedbackStage(false);
    const next = await drawWritingPrompt();
    if (next) {
      setPrompts((items) => newestFirst([...items, next], (item) => item.createdAt));
      setAnswer("");
      setPrompt(next);
    } else setError(true);
    setCreating(false);
  };

  const deletePrompt = async (id: string) => {
    if (!window.confirm("이 작문 문제를 삭제할까요?")) return;
    if (await deleteWritingPrompt(id)) {
      setPrompts((items) => items.filter((item) => item.id !== id));
    }
  };

  const backToList = () => {
    setPrompt(null);
    setAnswer("");
    setOnFeedbackStage(false);
    setCheckError(false);
    setError(false);
  };

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!prompt?.korean || !answer.trim() || checking || prompt.checkStatus === "pending") return;
    setChecking(true);
    setCheckError(false);
    const next = await checkWriting(prompt.id, answer.trim());
    // A null response means the server could not even accept the answer:
    // keep the draft in the editor and say so. Once accepted, the answer is
    // durably pending its grade — leaving the page now loses nothing.
    if (next) {
      adoptPrompt(next);
      if (next.checkStatus === "done") setOnFeedbackStage(true);
    } else setCheckError(true);
    setChecking(false);
  };

  const recheck = async () => {
    if (!prompt || checking || prompt.checkStatus === "pending") return;
    setChecking(true);
    setCheckError(false);
    setOnFeedbackStage(false);
    const next = await recheckWriting(prompt.id);
    if (next) adoptPrompt(next); else setCheckError(true);
    setChecking(false);
  };

  const grading = prompt?.checkStatus === "pending";
  const feedback = prompt?.checkStatus === "done"
    ? { original: prompt.answer ?? answer, corrected: prompt.corrected ?? "", issues: prompt.issues ?? [] }
    : null;

  return <LearningPage title="오늘의 작문" viewKey={prompt?.id ?? "list"}>
    {!prompt && (
      <>
        <LearningIntro eyebrow="생각을 영어로 옮기는 연습" title="한 문장부터 써 볼까요?" description="한국어 문장을 나만의 영어로 표현해 보세요. 답안을 확인하며 더 자연스러운 표현을 익혀요." steps={["문제 만들기", "영어로 쓰기", "피드백 살펴보기"]} />
        <PageToolbar>
          <button type="button" className="quiz-start-btn" onClick={() => void createPrompt()} disabled={creating}>
            {creating ? "만드는 중…" : "＋ 새 문제 만들기"}
          </button>
        </PageToolbar>
        <PageSection title="나의 작문 기록" description={state === "ready" ? `총 ${prompts.length}개 · 최근 기록부터` : undefined}>
          {state === "loading" && <LoadingHint />}
          {state === "ready" && prompts.length === 0 && <EmptyState title="아직 만든 작문 문제가 없어요." description="‘새 문제 만들기’로 시작해 보세요. 모르는 단어는 답안 옆에서 찾아볼 수 있어요." />}
          <DatedList items={prompts}>{(item) => (
            <HistoryItem
              title={item.korean || "문제를 만드는 중…"}
              badges={item.status !== "done"
                ? <span className="study-summary-pending-badge"><span className="spinning">⏳</span> 생성 중</span>
                : item.checkStatus === "pending" && <span className="study-summary-pending-badge"><span className="spinning">⏳</span> 검사 중</span>}
              meta={formatMessageTime(item.createdAt)}
              onOpen={() => void openPrompt(item)}
              onDelete={() => void deletePrompt(item.id)}
              deleteLabel="작문 문제 삭제"
            />
          )}</DatedList>
        </PageSection>
      </>
    )}

    {prompt && (
      <>
      <BackButton onClick={backToList} />
      <PageSection title="오늘의 한 문장" description="한국어 문장을 영어로 표현해 보세요." className="page-card writing-detail-card">
        {loadingPrompt ? <p className="hint">문제를 불러오는 중이에요…</p> : error ? <>
          <p className="hint">문제를 불러오지 못했어요.</p>
          <button type="button" onClick={() => void openPrompt(prompt)}>다시 시도</button>
        </> : <>
          {prompt.status === "pending" && <p className="hint">문제를 만드는 중이에요…</p>}
          {prompt.status === "failed" && <p className="hint">문제 생성에 실패했어요. 잠시 후 다시 확인해 주세요.</p>}
          {prompt.status === "done" && <>
            <div className="writing-stage">
              <div className="language-label">한글 문제</div>
              <p className="translation-quote writing-prompt" lang="ko">{prompt.korean}</p>
            </div>
            {feedback && onFeedbackStage ? <section className="writing-feedback" aria-label="답안 피드백" aria-live="polite">
              <div className={`quiz-result ${feedback.issues.length ? "similar" : "correct"}`} role="status">
                {feedback.issues.length ? "조금 다듬어 볼까요?" : "아주 좋아요!"}
              </div>
              <div className="writing-compare">
                <div className="writing-compare-item">
                  <div className="language-label">내가 쓴 문장</div>
                  <p className="writing-original">{feedback.original}</p>
                </div>
                <div className="writing-compare-item">
                  <div className="language-label">더 자연스러운 문장</div>
                  <p className="writing-corrected" lang="en">{feedback.corrected}</p>
                </div>
              </div>
              {feedback.issues.length > 0 && <ul className="writing-issues">
                {feedback.issues.map((issue, i) => (
                  <li className="writing-issue" key={i}>
                    <div className="writing-issue-head">
                      <span className="writing-issue-badge">{issueLabel(issue.type)}</span>
                      <p className="writing-issue-swap"><span className="writing-issue-from">{issue.span}</span> <span aria-hidden="true">→</span> <strong>{issue.suggestion}</strong></p>
                    </div>
                    <p className="writing-issue-note">{issue.explanationTranslation || issue.explanation}</p>
                  </li>
                ))}
              </ul>}
              <PageToolbar label="답안 관리">
                <button type="button" className="ghost" onClick={() => setOnFeedbackStage(false)}>다시 수정하기</button>
                <button type="button" className="ghost" onClick={() => void recheck()} disabled={checking}>
                  {checking || grading ? "검사 중…" : "다시 검사"}
                </button>
                <button type="button" className="quiz-start-btn" onClick={() => void createPrompt()} disabled={creating}>
                  {creating ? "만드는 중…" : "다른 문장 받기"}
                </button>
              </PageToolbar>
            </section> : <>
              {checkError && <p className="writing-check-error" role="alert">답안을 확인하지 못했어요. 연결 상태를 확인한 뒤 다시 시도해 주세요.</p>}
              {prompt.checkStatus === "failed" && <p className="writing-check-error" role="alert">답안 확인에 실패했어요. ‘다시 검사’로 다시 시도해 주세요.</p>}
              {/* Keep the word search beside the answer form, not inside it:
                  WordSearchControl owns its own search <form>, and nested
                  forms make Enter in that field submit the writing answer in
                  some browsers. */}
              <div className="writing-answer-tools">
                <span className="hint">모르는 단어가 있으면 검색해 보세요.</span>
                <WordSearchControl placement="below" />
              </div>
              <form className="writing-answer-form" onSubmit={submit}>
                <label className="language-label" htmlFor="writing-answer">영어 답안</label>
                <textarea id="writing-answer" value={answer} onChange={(e) => setAnswer(e.target.value)} placeholder="영어로 한 문장을 써보세요" rows={4} disabled={checking || grading} />
                {grading
                  ? <p className="hint" role="status">답안을 저장했어요. 결과를 확인하는 중이며, 페이지를 떠나도 계속 진행돼요.</p>
                  : prompt.answer && !checkError && prompt.checkStatus !== "failed" && <p className="hint">지난 답안을 불러왔어요. 그대로 확인하거나 수정해서 제출하세요.</p>}
                <button type="submit" disabled={checking || grading || !answer.trim()}>{grading ? "검사 중…" : "답안 확인"}</button>
                {!grading && prompt.checkStatus === "failed" && (
                  <button type="button" className="quiz-start-btn" onClick={() => void recheck()} disabled={checking}>다시 검사</button>
                )}
              </form>
            </>}
          </>}
        </>}
      </PageSection>
      </>
    )}
  </LearningPage>;
}
