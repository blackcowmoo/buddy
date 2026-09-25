import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { LoadingHint } from "../components/LoadingHint";
import { NuanceQuestionCard } from "../components/NuanceQuestionCard";
import { SubPageHeader } from "../components/SubPageHeader";
import { dueQuestions, fetchNuanceLesson, fetchNuanceLessons, practiceNuance, startNuanceReview, type NuanceAction, type NuanceLesson, type NuanceReviewItem } from "../lib/nuance";

const reviewItemParam = "item";

export function readNuanceReviewOrder(search = window.location.search): NuanceReviewItem[] {
  const items: NuanceReviewItem[] = [];
  for (const raw of new URLSearchParams(search).getAll(reviewItemParam)) {
    try {
      const [lessonId, questionId] = JSON.parse(raw) as unknown[];
      if (typeof lessonId === "string" && typeof questionId === "string") items.push({ lessonId, questionId });
    } catch { /* Ignore a malformed bookmark and draw a fresh batch. */ }
  }
  return items;
}

function replaceNuanceReviewOrder(items: NuanceReviewItem[]) {
  const url = new URL(window.location.href);
  url.searchParams.delete(reviewItemParam);
  for (const item of items) url.searchParams.append(reviewItemParam, JSON.stringify([item.lessonId, item.questionId]));
  window.history.replaceState(null, "", url);
}

function nuanceListPath() {
  return window.location.pathname.replace(/\/nuance-review\/?$/, "/nuance");
}

export function WordNuanceReview() {
  const [lessons, setLessons] = useState<NuanceLesson[]>([]);
  const [order, setOrder] = useState<NuanceReviewItem[]>(() => readNuanceReviewOrder());
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const started = useRef(false);
  const busyRef = useRef(false);

  const remember = useCallback((lesson: NuanceLesson) => {
    setLessons((items) => items.map((item) => item.id === lesson.id && item.revision <= lesson.revision ? lesson : item));
  }, []);

  const begin = useCallback(async () => {
    if (busyRef.current) return;
    busyRef.current = true; setBusy(true); setError(null);
    const batch = await startNuanceReview();
    busyRef.current = false; setBusy(false); setLoading(false);
    if (!batch) { setError("복습 문제를 준비하지 못했어요. 다시 시도해 주세요."); return; }
    setLessons(batch.lessons);
    setOrder(batch.items);
    replaceNuanceReviewOrder(batch.items);
  }, []);

  useEffect(() => {
    if (started.current) return;
    started.current = true;
    const saved = readNuanceReviewOrder();
    if (!saved.length) { void begin(); return; }
    void fetchNuanceLessons().then((items) => {
      setLoading(false);
      if (items) setLessons(items);
      else setError("복습 진행을 불러오지 못했어요. 다시 시도해 주세요.");
    });
  }, [begin]);

  const current = useMemo(() => {
    for (const item of order) {
      const lesson = lessons.find((candidate) => candidate.id === item.lessonId);
      if (!lesson) continue;
      if (lesson.state.feedback?.questionId === item.questionId || lesson.state.queue[0] === item.questionId) {
        const question = lesson.content?.questions.find((candidate) => candidate.id === item.questionId);
        if (question) return { item, lesson, question };
      }
    }
    return null;
  }, [lessons, order]);

  const remaining = order.filter((item) => {
    const lesson = lessons.find((candidate) => candidate.id === item.lessonId);
    return lesson?.state.feedback?.questionId === item.questionId || lesson?.state.queue.includes(item.questionId);
  }).length;
  const due = lessons.reduce((sum, lesson) => sum + dueQuestions(lesson), 0);

  const act = (kind: NuanceAction["kind"], extra: Partial<NuanceAction> = {}) => {
    if (!current || busyRef.current) return;
    const { lesson, item } = current;
    const missed = kind === "next" && lesson.state.feedback?.correct === false;
    busyRef.current = true; setBusy(true); setError(null);
    void practiceNuance(lesson.id, { ...extra, kind, revision: lesson.revision }).then(async (updated) => {
      if (updated) {
        remember(updated);
        if (missed) {
          setOrder((items) => {
            const next = [...items.filter((candidate) => candidate !== item), item];
            replaceNuanceReviewOrder(next);
            return next;
          });
        }
        return;
      }
      const restored = await fetchNuanceLesson(lesson.id);
      if (restored) remember(restored);
      setError(restored ? "진행이 변경되어 서버의 최신 상태를 불러왔어요." : "진행을 저장하지 못했어요. 다시 시도해 주세요.");
    }).finally(() => { busyRef.current = false; setBusy(false); });
  };

  return <div className="app">
    <SubPageHeader title="단어 뉘앙스 복습" />
    <main className="convo nuance-page">
      <a className="ghost-link nuance-list-back" href={nuanceListPath()}>← 학습 목록</a>
      {error && <p role="alert">{error}</p>}
      {loading && <LoadingHint />}
      {!loading && current && <>
        <p className="hint">이번 복습 {order.length}문제 · 남은 문제 {remaining}개</p>
        <NuanceQuestionCard
          key={`${current.lesson.id}-${current.question.id}-${current.lesson.state.progress[current.question.id]?.attempts ?? 0}`}
          lesson={current.lesson}
          question={current.question}
          busy={busy}
          onAnswer={(selected) => act("answer", { questionId: current.question.id, selected })}
        />
        {current.lesson.state.feedback && <div className="nuance-review">
          <button type="button" disabled={busy} onClick={() => act("next")}>{busy ? "저장 중…" : "다음 문맥"}</button>
          {current.lesson.state.feedback.correct && <button type="button" className="ghost" disabled={busy} onClick={() => act("next", { repeat: true })}>맞혔지만 다시 복습</button>}
        </div>}
      </>}
      {!loading && !current && !error && <section className="quiz-panel" aria-live="polite">
        <h2>{order.length ? `이번 ${order.length}문제 복습을 마쳤어요` : "지금 복습할 문제가 없어요"}</h2>
        <p>모든 비교 묶음의 문맥을 섞어서 복습했어요.</p>
        {due > 0 && <button type="button" disabled={busy} onClick={() => void begin()}>{busy ? "준비 중…" : "다음 5문제 복습"}</button>}
        <a className="ghost-link" href={nuanceListPath()}>학습 목록 보기</a>
      </section>}
    </main>
  </div>;
}
