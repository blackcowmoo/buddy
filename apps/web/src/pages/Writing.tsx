import { Fragment, useCallback, useEffect, useState } from "react";
import type { Correction } from "../lib/protocol";
import { LoadingHint } from "../components/LoadingHint";
import { SubPageHeader } from "../components/SubPageHeader";
import { WordSearchControl } from "../components/WordSearchControl";
import { formatDateDivider, formatMessageTime, shouldShowDateDivider } from "../lib/time";
import { checkWriting, deleteWritingPrompt, drawWritingPrompt, fetchWritingPrompt, fetchWritingPrompts, type WritingPrompt } from "../lib/writing";

type LoadState = "loading" | "ready";

// The writing page follows the same two-level flow as ArticleQuiz: the root
// view is the learner's problem history, and opening/drawing a problem moves
// to a focused answer view. Keeping these states separate makes it possible
// to browse old prompts without putting the answer form beside the list.
export function Writing() {
  const [state, setState] = useState<LoadState>("loading");
  const [prompts, setPrompts] = useState<WritingPrompt[]>([]);
  const [prompt, setPrompt] = useState<WritingPrompt | null>(null);
  const [answer, setAnswer] = useState("");
  const [result, setResult] = useState<Correction | null>(null);
  const [loadingPrompt, setLoadingPrompt] = useState(false);
  const [creating, setCreating] = useState(false);
  const [checking, setChecking] = useState(false);
  const [error, setError] = useState(false);

  const loadPrompts = useCallback(async () => {
    setState("loading");
    const list = await fetchWritingPrompts();
    setPrompts(list.slice().sort((a, b) => a.createdAt - b.createdAt));
    setState("ready");
  }, []);

  useEffect(() => { void loadPrompts(); }, [loadPrompts]);

  const openPrompt = useCallback(async (item: WritingPrompt) => {
    setLoadingPrompt(true);
    setError(false);
    setResult(null);
    setAnswer("");
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
    const next = await drawWritingPrompt();
    if (next) {
      setPrompts((items) => [...items, next].sort((a, b) => a.createdAt - b.createdAt));
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
    setError(false);
    void loadPrompts();
  };

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!prompt?.korean || !answer.trim() || checking) return;
    setChecking(true);
    setResult(await checkWriting(prompt.korean, answer));
    setChecking(false);
  };

  return <div className="app">
    <SubPageHeader title="오늘의 작문" />
    <main className="convo writing-page">
      {!prompt && (
        <>
          {state === "loading" && <LoadingHint />}
          {state === "ready" && prompts.length === 0 && <p className="hint">아직 만든 작문 문제가 없어요.</p>}
          {prompts.map((item, i) => {
            const prev = prompts[i - 1];
            const showDivider = shouldShowDateDivider(prev?.createdAt, item.createdAt);
            return (
              <Fragment key={item.id}>
                {showDivider && (
                  <div className="date-divider">
                    <span>{formatDateDivider(item.createdAt)}</span>
                  </div>
                )}
                <div className="writing-list-row">
                  <button type="button" className="writing-list-open" onClick={() => void openPrompt(item)}>
                    <span className="writing-list-text">{item.korean || "문제를 만드는 중…"}</span>
                    <span className="writing-list-meta">
                      {item.status !== "done" && <span className="study-summary-pending-badge"><span className="spinning">⏳</span> 생성 중</span>}
                      {formatMessageTime(item.createdAt)}
                    </span>
                  </button>
                  <button type="button" className="ghost icon-btn writing-delete" onClick={() => void deletePrompt(item.id)} aria-label="작문 문제 삭제" title="작문 문제 삭제">🗑</button>
                </div>
              </Fragment>
            );
          })}
          <button type="button" className="quiz-start-btn" onClick={() => void createPrompt()} disabled={creating}>
            {creating ? "만드는 중…" : "＋ 새 문제 만들기"}
          </button>
        </>
      )}

      {prompt && (
        <div className="writing-card writing-detail-card">
          <button type="button" className="ghost quiz-back-btn" onClick={backToList}>← 목록으로</button>
          <p className="eyebrow">KOREAN → ENGLISH</p>
          <h2>오늘의 한 문장</h2>
          {loadingPrompt ? <p className="hint">문제를 불러오는 중이에요…</p> : error ? <>
            <p className="hint">문제를 불러오지 못했어요.</p>
            <button type="button" onClick={() => void openPrompt(prompt)}>다시 시도</button>
          </> : <>
            {prompt.status === "pending" && <p className="hint">문제를 만드는 중이에요…</p>}
            {prompt.status === "failed" && <p className="hint">문제 생성에 실패했어요. 잠시 후 다시 확인해 주세요.</p>}
            {prompt.status === "done" && <>
              <p className="writing-prompt">{prompt.korean}</p>
              <form onSubmit={submit}>
                <div className="writing-answer-tools">
                  <WordSearchControl />
                  <span className="hint">모르는 단어가 있으면 검색해 보세요.</span>
                </div>
                <textarea value={answer} onChange={(e) => setAnswer(e.target.value)} placeholder="영어로 한 문장을 써보세요" rows={3} disabled={checking} />
                <button type="submit" disabled={checking || !answer.trim()}>{checking ? "검사 중…" : "답안 확인"}</button>
              </form>
            </>}
            {result && <section className="writing-feedback" aria-live="polite">
              <h3>{result.issues.length ? "조금 다듬어 볼까요?" : "아주 좋아요!"}</h3>
              <p className="writing-corrected">{result.corrected}</p>
              {result.issues.map((issue, i) => <div className="writing-issue" key={i}><strong>{issue.suggestion}</strong><p>{issue.explanationTranslation || issue.explanation}</p></div>)}
            </section>}
          </>}
        </div>
      )}
    </main>
  </div>;
}
