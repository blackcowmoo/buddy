import { useCallback, useEffect, useState } from "react";
import type { Correction } from "../lib/protocol";
import { checkWriting, drawWritingPrompt, fetchWritingPrompt, fetchWritingPrompts, type WritingPrompt } from "../lib/writing";

export function Writing() {
  const [prompt, setPrompt] = useState<WritingPrompt | null>(null);
  const [prompts, setPrompts] = useState<WritingPrompt[]>([]);
  const [answer, setAnswer] = useState("");
  const [result, setResult] = useState<Correction | null>(null);
  const [loading, setLoading] = useState(true);
  const [checking, setChecking] = useState(false);
  const [error, setError] = useState(false);

  const openPrompt = useCallback(async (item: WritingPrompt) => {
    setLoading(true); setError(false); setResult(null); setAnswer("");
    const next = await fetchWritingPrompt(item.id);
    if (!next) setError(true); else setPrompt(next);
    setLoading(false);
  }, []);
  const loadPrompts = useCallback(async () => {
    setLoading(true);
    const list = await fetchWritingPrompts();
    setPrompts(list);
    if (list.length) await openPrompt(list[0]);
    else setLoading(false);
  }, [openPrompt]);
  useEffect(() => { void loadPrompts(); }, [loadPrompts]);

  useEffect(() => {
    if (!prompt || prompt.status === "done") return;
    const timer = window.setInterval(async () => {
      const next = await fetchWritingPrompt(prompt.id);
      if (next) { setPrompt(next); setPrompts(items => items.map(item => item.id === next.id ? next : item)); if (next.status === "done") window.clearInterval(timer); }
    }, 2500);
    return () => window.clearInterval(timer);
  }, [prompt]);

  const loadPrompt = async () => {
    setResult(null); setAnswer(""); setError(false); setLoading(true);
    const next = await drawWritingPrompt();
    if (!next) setError(true); else { setPrompts(items => [next, ...items]); setPrompt(next); }
    setLoading(false);
  };

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!prompt?.korean || !answer.trim()) return;
    setChecking(true); setResult(null);
    setResult(await checkWriting(prompt.korean, answer));
    setChecking(false);
  };

  return <div className="app writing-page">
    <header className="topbar"><div className="brand"><button className="ghost" onClick={() => window.location.assign(".")}>←</button><h1>오늘의 작문</h1></div></header>
    <main className="writing-card">
      <aside><strong>문제 목록</strong>{prompts.map(item => <button className="ghost" key={item.id} onClick={() => void openPrompt(item)}>{item.korean || "만드는 중…"}</button>)}<button onClick={() => void loadPrompt()}>＋ 새 문제 만들기</button></aside>
      <p className="eyebrow">KOREAN → ENGLISH</p>
      <h2>오늘의 한 문장</h2>
      {loading ? <p className="hint">문제를 만드는 중이에요…</p> : error ? <><p className="hint">문제를 불러오지 못했어요.</p><button onClick={() => void loadPrompt()}>다시 시도</button></> : <>
        {prompt.status === "pending" ? <p className="hint">문제를 만드는 중이에요…</p> : prompt.status === "failed" ? <p className="hint">문제 생성에 실패했어요. 잠시 후 다시 확인해 주세요.</p> : <p className="writing-prompt">{prompt.korean}</p>}
        {prompt.status === "done" && <form onSubmit={submit}>
          <textarea value={answer} onChange={e => setAnswer(e.target.value)} placeholder="영어로 한 문장을 써보세요" rows={3} disabled={checking} />
          <button type="submit" disabled={checking || !answer.trim()}>{checking ? "검사 중…" : "답안 확인"}</button>
        </form>}
        {result && <section className="writing-feedback" aria-live="polite">
          <h3>{result.issues.length ? "조금 다듬어 볼까요?" : "아주 좋아요!"}</h3>
          <p className="writing-corrected">{result.corrected}</p>
          {result.issues.map((issue, i) => <div className="writing-issue" key={i}><strong>{issue.suggestion}</strong><p>{issue.explanationTranslation || issue.explanation}</p></div>)}
          <button className="ghost" onClick={() => void loadPrompt()}>새 문제 만들기</button>
        </section>}
      </>}
    </main>
  </div>;
}
