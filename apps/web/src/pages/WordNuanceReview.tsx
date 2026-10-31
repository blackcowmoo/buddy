import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { LoadingHint } from "../components/LoadingHint";
import { NuanceQuestionCard } from "../components/NuanceQuestionCard";
import { LearningIntro } from "../components/LearningIntro";
import { LearningPage } from "../components/LearningPage";
import { BackButton, PageSection, PageToolbar } from "../components/PageLayout";
import { dueQuestions, fetchNuanceLesson, fetchNuanceReview, practiceNuance, startNuanceReview, type NuanceAction, type NuanceLesson, type NuanceReviewItem } from "../lib/nuance";

function nuanceListPath() {
  return window.location.pathname.replace(/\/nuance-review\/?$/, "/nuance");
}

export function WordNuanceReview() {
  const [lessons, setLessons] = useState<NuanceLesson[]>([]);
  const [order, setOrder] = useState<NuanceReviewItem[]>([]);
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
  }, []);

  useEffect(() => {
    if (started.current) return;
    started.current = true;
    // The server owns the review batch, so a reopen replays the saved batch
    // instead of reading question lists back from the URL. Only when no batch
    // is active does the page draw a new one.
    void fetchNuanceReview().then((saved) => {
      if (saved && saved.items.length) {
        setLessons(saved.lessons);
        setOrder(saved.items);
        setLoading(false);
        return;
      }
      return begin();
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
        // Mirrors the server's persisted move of a missed question to the end
        // of the batch; the response stays authoritative on the next load.
        if (missed) setOrder((items) => [...items.filter((candidate) => candidate !== item), item]);
        return;
      }
      const restored = await fetchNuanceLesson(lesson.id);
      if (restored) remember(restored);
      setError(restored ? "진행이 변경되어 서버의 최신 상태를 불러왔어요." : "진행을 저장하지 못했어요. 다시 시도해 주세요.");
    }).finally(() => { busyRef.current = false; setBusy(false); });
  };

  return <LearningPage title="단어 뉘앙스 복습">
    <BackButton href={nuanceListPath()}>학습 목록</BackButton>
    <LearningIntro eyebrow="문맥으로 다시 익히는 표현" title="익힌 표현을 다시 만나 봐요" description="여러 비교 묶음의 문맥을 섞어서 복습해요. 상황과 의도에 가장 잘 맞는 단어를 골라 보세요." />
    <PageSection title="오늘의 뉘앙스 복습">
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
        {current.lesson.state.feedback && <PageToolbar label="복습 진행">
          <button type="button" disabled={busy} onClick={() => act("next")}>{busy ? "저장 중…" : "다음 문맥"}</button>
          {current.lesson.state.feedback.correct && <button type="button" className="ghost" disabled={busy} onClick={() => act("next", { repeat: true })}>맞혔지만 다시 복습</button>}
        </PageToolbar>}
      </>}
      {!loading && !current && !error && <section className="quiz-panel" aria-live="polite">
        <h2>{order.length ? `이번 ${order.length}문제 복습을 마쳤어요` : "지금 복습할 문제가 없어요"}</h2>
        <p>모든 비교 묶음의 문맥을 섞어서 복습했어요.</p>
        {due > 0 && <button type="button" disabled={busy} onClick={() => void begin()}>{busy ? "준비 중…" : "지금 나온 문제 모두 복습"}</button>}
        <a className="ghost-link" href={nuanceListPath()}>학습 목록 보기</a>
      </section>}
    </PageSection>
  </LearningPage>;
}
