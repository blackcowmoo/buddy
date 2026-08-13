import { useEffect, useState } from "react";
import type { Correction } from "../lib/protocol";
import { checkWriting, fetchWritingPrompt } from "../lib/writing";

export function Writing() {
  const [prompt, setPrompt] = useState<string | null>(null);
  const [answer, setAnswer] = useState("");
  const [result, setResult] = useState<Correction | null>(null);
  const [loading, setLoading] = useState(true);
  const [checking, setChecking] = useState(false);
  const [error, setError] = useState(false);

  const loadPrompt = async () => {
    setLoading(true); setError(false); setResult(null); setAnswer("");
    const next = await fetchWritingPrompt();
    if (!next) setError(true); else setPrompt(next.korean);
    setLoading(false);
  };
  useEffect(() => { void loadPrompt(); }, []);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!prompt || !answer.trim()) return;
    setChecking(true); setResult(null);
    setResult(await checkWriting(prompt, answer));
    setChecking(false);
  };

  return <div className="app writing-page">
    <header className="topbar"><div className="brand"><button className="ghost" onClick={() => window.location.assign(".")}>←</button><h1>오늘의 작문</h1></div></header>
    <main className="writing-card">
      <p className="eyebrow">KOREAN → ENGLISH</p>
      <h2>오늘의 한 문장</h2>
      {loading ? <p className="hint">문제를 만드는 중이에요…</p> : error ? <><p className="hint">문제를 불러오지 못했어요.</p><button onClick={() => void loadPrompt()}>다시 시도</button></> : <>
        <p className="writing-prompt">{prompt}</p>
        <form onSubmit={submit}>
          <textarea value={answer} onChange={e => setAnswer(e.target.value)} placeholder="영어로 한 문장을 써보세요" rows={3} disabled={checking} />
          <button type="submit" disabled={checking || !answer.trim()}>{checking ? "검사 중…" : "답안 확인"}</button>
        </form>
        {result && <section className="writing-feedback" aria-live="polite">
          <h3>{result.issues.length ? "조금 다듬어 볼까요?" : "아주 좋아요!"}</h3>
          <p className="writing-corrected">{result.corrected}</p>
          {result.issues.map((issue, i) => <div className="writing-issue" key={i}><strong>{issue.suggestion}</strong><p>{issue.explanationTranslation || issue.explanation}</p></div>)}
          <button className="ghost" onClick={() => void loadPrompt()}>새 문제 풀기</button>
        </section>}
      </>}
    </main>
  </div>;
}
