import { useCallback, useEffect, useState } from "react";
import { EmptyState, LearningIntro } from "../components/LearningIntro";
import type { Correction } from "../lib/protocol";
import { LoadingHint } from "../components/LoadingHint";
import { LearningPage } from "../components/LearningPage";
import { BackButton, PageSection, PageToolbar } from "../components/PageLayout";
import { DatedList } from "../components/DatedList";
import { HistoryItem } from "../components/HistoryItem";
import { WordSearchControl } from "../components/WordSearchControl";
import { formatMessageTime } from "../lib/time";
import { checkWriting, deleteWritingPrompt, drawWritingPrompt, fetchWritingPrompt, fetchWritingPrompts, type WritingPrompt } from "../lib/writing";
import { newestFirst } from "../lib/listView";

type LoadState = "loading" | "ready";

// Issue.type comes from the Judge's correction schema (lib/protocol.ts);
// an unknown value falls back to the generic label so a new server-side
// type never renders as an empty badge.
const issueTypeLabels: Record<string, string> = {
  grammar: "문법",
  vocabulary: "어휘",
  phrasing: "표현",
  context: "맥락",
};
const issueTypeLabel = (type: string) => issueTypeLabels[type] ?? "교정";

// The writing page follows ArticleQuiz's staged flow: the root view is the
// learner's problem history, opening/drawing a problem shows the answer
// editor, and a checked answer swaps the editor for the feedback stage —
// one focus per screen, the same shape the other exercise pages use.
export function Writing() {
  const [state, setState] = useState<LoadState>("loading");
  const [prompts, setPrompts] = useState<WritingPrompt[]>([]);
  const [prompt, setPrompt] = useState<WritingPrompt | null>(null);
  const [answer, setAnswer] = useState("");
  const [result, setResult] = useState<Correction | null>(null);
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

  const openPrompt = useCallback(async (item: WritingPrompt) => {
    setLoadingPrompt(true);
    setError(false);
    setResult(null);
    setAnswer("");
    setCheckError(false);
    const next = await fetchWritingPrompt(item.id);
    if (next) setPrompt(next); else setError(true);
    setLoadingPrompt(false);
  }, []);

  useEffect(() => {
    if (!prompt || prompt.status === "done") return;
    const timer = window.setInterval(async () => {
      const next = await fetchWritingPrompt(prompt.id);
      if (!next) return;
      setPrompt(next);
      setPrompts((items) => items.map((item) => item.id === next.id ? next : item));
      if (next.status === "done") window.clearInterval(timer);
    }, 2500);
    return () => window.clearInterval(timer);
  }, [prompt]);

  const createPrompt = async () => {
    setCreating(true);
    setError(false);
    setResult(null);
    setAnswer("");
    setCheckError(false);
    const next = await drawWritingPrompt();
    if (next) {
      setPrompts((items) => newestFirst([...items, next], (item) => item.createdAt));
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
    setResult(null);
    setAnswer("");
    setCheckError(false);
    setError(false);
    void loadPrompts();
  };

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!prompt?.korean || !answer.trim() || checking) return;
    setChecking(true);
    setCheckError(false);
    const next = await checkWriting(prompt.korean, answer, prompt.id);
    // A null response means the check itself failed: keep the draft in the
    // editor and say so, instead of silently redrawing the same empty form.
    if (next) setResult(next); else setCheckError(true);
    setChecking(false);
  };

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
              badges={item.status !== "done" && <span className="study-summary-pending-badge"><span className="spinning">⏳</span> 생성 중</span>}
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
              <p className="writing-label">한글 문제</p>
              <p className="writing-prompt" lang="ko">{prompt.korean}</p>
            </div>
            {!result && <>
              {checkError && <p className="writing-check-error" role="alert">답안을 확인하지 못했어요. 연결 상태를 확인한 뒤 다시 시도해 주세요.</p>}
              {/* Keep the word search beside the answer form, not inside it:
                  WordSearchControl owns its own search <form>, and nested
                  forms make Enter in that field submit the writing answer in
                  some browsers. */}
              <div className="writing-answer-tools">
                <span className="hint">모르는 단어가 있으면 검색해 보세요.</span>
                <WordSearchControl placement="below" />
              </div>
              <form className="writing-answer-form" onSubmit={submit}>
                <label className="writing-label" htmlFor="writing-answer">영어 답안</label>
                <textarea id="writing-answer" value={answer} onChange={(e) => setAnswer(e.target.value)} placeholder="영어로 한 문장을 써보세요" rows={4} disabled={checking} />
                <button type="submit" disabled={checking || !answer.trim()}>{checking ? "검사 중…" : "답안 확인"}</button>
              </form>
            </>}
            {result && <section className="writing-feedback" aria-label="답안 피드백" aria-live="polite">
              <div className={`quiz-result ${result.issues.length ? "similar" : "correct"}`} role="status">
                {result.issues.length ? "조금 다듬어 볼까요?" : "아주 좋아요!"}
              </div>
              <div className="writing-compare">
                <div className="writing-compare-item">
                  <p className="writing-label">내가 쓴 문장</p>
                  <p className="writing-original">{result.original || answer}</p>
                </div>
                <div className="writing-compare-item">
                  <p className="writing-label">더 자연스러운 문장</p>
                  <p className="writing-corrected" lang="en">{result.corrected}</p>
                </div>
              </div>
              {result.issues.length > 0 && <ul className="writing-issues">
                {result.issues.map((issue, i) => (
                  <li className="writing-issue" key={i}>
                    <div className="writing-issue-head">
                      <span className="writing-issue-badge">{issueTypeLabel(issue.type)}</span>
                      <p className="writing-issue-swap"><span className="writing-issue-from">{issue.span}</span> <span aria-hidden="true">→</span> <strong>{issue.suggestion}</strong></p>
                    </div>
                    <p className="writing-issue-note">{issue.explanationTranslation || issue.explanation}</p>
                  </li>
                ))}
              </ul>}
              <PageToolbar label="답안 관리">
                <button type="button" className="ghost" onClick={() => setResult(null)}>다시 수정하기</button>
                <button type="button" className="quiz-start-btn" onClick={() => void createPrompt()} disabled={creating}>
                  {creating ? "만드는 중…" : "다른 문장 받기"}
                </button>
              </PageToolbar>
            </section>}
          </>}
        </>}
      </PageSection>
      </>
    )}
  </LearningPage>;
}
