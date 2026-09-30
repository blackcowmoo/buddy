import { useCallback, useEffect, useRef, useState } from "react";
import { LearningPage } from "../components/LearningPage";
import { BackButton, PageSection, PageToolbar } from "../components/PageLayout";
import { EmptyState, LearningIntro } from "../components/LearningIntro";
import { DatedList } from "../components/DatedList";
import { HistoryItem } from "../components/HistoryItem";
import { LoadingHint } from "../components/LoadingHint";
import { NuanceQuestionCard } from "../components/NuanceQuestionCard";
import { deleteNuanceLesson, drawNuanceLesson, dueQuestions, fetchNuanceLesson, fetchNuanceLessons, lessonTitle, nextReview, practiceNuance, retryNuanceLesson, type NuanceAction, type NuanceLesson } from "../lib/nuance";
import { formatAbsoluteDateTime, formatMessageTime } from "../lib/time";
import { newestFirst } from "../lib/listView";

const selectedID = () => new URLSearchParams(window.location.search).get("lesson");
function navigate(id: string | null) {
  const url = new URL(window.location.href);
  if (id) url.searchParams.set("lesson", id); else url.searchParams.delete("lesson");
  window.history.pushState(null, "", url);
}
const generating = (l: NuanceLesson) => l.status === "pending" || l.status === "processing";

function lessonStatus(lesson: NuanceLesson): string {
  if (generating(lesson)) return "생성 중";
  if (lesson.status === "failed") return "생성 실패 · 다시 시도";
  if (lesson.state.queue.length) return `학습 중 · 남은 문맥 ${lesson.state.queue.length}개`;
  const due = dueQuestions(lesson);
  return due ? `복습할 문맥 ${due}개` : `복습 완료 · 다음 ${formatAbsoluteDateTime(nextReview(lesson)!)}`;
}

export function WordNuance() {
  const [lessons, setLessons] = useState<NuanceLesson[]>([]);
  const [selected, setSelected] = useState<NuanceLesson | null>(null);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [practicing, setPracticing] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const busyRef = useRef(false);
  const navigation = useRef(0);

  const remember = useCallback((lesson: NuanceLesson) => {
    setLessons((items) => newestFirst(
      items.some((l) => l.id === lesson.id) ? items.map((l) => l.id === lesson.id && l.revision <= lesson.revision ? lesson : l) : [...items, lesson],
      (item) => item.createdAt,
    ));
    setSelected((old) => old?.id === lesson.id && old.revision <= lesson.revision ? lesson : old);
  }, []);

  const load = useCallback(async () => {
    setLoading(true);
    const items = await fetchNuanceLessons();
    if (items) { setLessons(newestFirst(items, (item) => item.createdAt)); setError(null); }
    else setError("학습 목록을 불러오지 못했어요. 다시 시도해 주세요.");
    setLoading(false);
  }, []);

  const open = useCallback(async (id: string, push = true) => {
    const token = ++navigation.current;
    setLoading(true); setError(null);
    let lesson = await fetchNuanceLesson(id);
    if (token !== navigation.current) return;
    setLoading(false);
    if (!lesson) { setError("문제를 불러오지 못했어요. 목록에서 다시 열어 주세요."); return; }
    remember(lesson); setSelected(lesson); setPracticing(lesson.state.queue.length > 0);
    if (push) navigate(id);
  }, [remember]);

  useEffect(() => {
    void load();
    const id = selectedID();
    if (id) void open(id, false);
    const pop = () => {
      const next = selectedID();
      if (next) void open(next, false);
      else { navigation.current++; setSelected(null); setError(null); setLoading(false); void load(); }
    };
    window.addEventListener("popstate", pop);
    return () => { navigation.current++; window.removeEventListener("popstate", pop); };
  }, [load, open]);

  // Watch the persisted list even while a different lesson is open. A slow
  // response from an old poll cannot replace a newer answer or deleted item.
  useEffect(() => {
    if (!lessons.some(generating) || busy) return;
    let cancelled = false;
    const timer = window.setTimeout(async () => {
      const items = await fetchNuanceLessons();
      if (cancelled) return;
      if (items) { setLessons(newestFirst(items, (item) => item.createdAt)); setSelected((old) => old ? items.find((l) => l.id === old.id && l.revision >= old.revision) ?? old : null); }
      else setLessons((current) => [...current]);
    }, 2500);
    return () => { cancelled = true; window.clearTimeout(timer); };
  }, [lessons, busy]);

  const run = async (work: () => Promise<void>) => {
    if (busyRef.current) return;
    busyRef.current = true; setBusy(true); setError(null);
    try { await work(); } finally { busyRef.current = false; setBusy(false); }
  };
  const act = (kind: NuanceAction["kind"], extra: Partial<NuanceAction> = {}) => {
    if (!selected) return;
    const lesson = selected;
    const token = navigation.current;
    void run(async () => {
      const next = await practiceNuance(lesson.id, { ...extra, kind, revision: lesson.revision });
      if (next) { remember(next); if (kind === "start" && token === navigation.current) setPracticing(true); }
      else {
        // A response can be lost after commit, or another device can win the
        // revision. Always restore server state before enabling another answer.
        const current = await fetchNuanceLesson(lesson.id);
        if (current) remember(current);
        if (token === navigation.current) setError(current ? "진행이 변경되었거나 저장 응답을 받지 못했어요. 서버의 최신 진행을 불러왔어요." : "진행 저장을 확인하지 못했어요. 연결 상태를 확인한 뒤 다시 시도해 주세요.");
      }
    });
  };
  const create = () => {
    const token = navigation.current;
    void run(async () => {
      const lesson = await drawNuanceLesson();
      if (!lesson) { setError("문제를 만들지 못했어요. 다시 시도해 주세요."); return; }
      remember(lesson);
      if (token !== navigation.current) return;
      navigation.current++; setSelected(lesson); setPracticing(false); navigate(lesson.id);
    });
  };
  const back = () => { navigation.current++; setSelected(null); setError(null); setLoading(false); navigate(null); void load(); };
  const remove = (id: string) => {
    if (!window.confirm("이 비교 묶음과 풀이 기록을 삭제할까요?")) return;
    void run(async () => {
      if (await deleteNuanceLesson(id)) setLessons((items) => items.filter((l) => l.id !== id));
      else setError("삭제하지 못했어요. 다시 시도해 주세요.");
    });
  };
  const due = lessons.reduce((sum, l) => sum + dueQuestions(l), 0);
  const hasReview = lessons.some((l) => l.state.queue.length > 0 || dueQuestions(l) > 0);
  const content = selected?.content;
  const question = content?.questions.find((q) => q.id === selected?.state.queue[0]);
  const next = selected ? nextReview(selected) : null;

  return <LearningPage title="단어 뉘앙스" viewKey={selected?.id ?? "list"}>
    {!selected ? <>
      <LearningIntro eyebrow="같은 뜻, 미묘하게 다른 느낌" title="상황에 맞는 단어를 익혀요" description="서로 바꿔 써도 기본 뜻이 통하는 단어들을 비교해요. 상황과 의도에 따라 달라지는 말투와 느낌을 익혀 보세요." />
      <PageToolbar>
        <button type="button" onClick={create} disabled={busy || loading}>{busy ? "처리 중…" : "＋ 새 문제 만들기"}</button>
        {hasReview && <a className="nuance-review-link" aria-disabled={busy || loading} onClick={(event) => { if (busy || loading) event.preventDefault(); }} href={window.location.pathname.replace(/\/nuance\/?$/, "/nuance-review")}>복습 시작</a>}
      </PageToolbar>
      <PageSection title="나의 뉘앙스 학습 기록" description={`${lessons.length}개 묶음 · 복습할 문맥 ${due}개`}>
        {error && <p role="alert">{error}</p>}
        {loading && <LoadingHint />}
        {!loading && error && <button type="button" className="ghost" onClick={() => void load()}>목록 다시 불러오기</button>}
        {!loading && !error && lessons.length === 0 && <EmptyState title="아직 만든 문제가 없어요." description="첫 비교 묶음을 만들어 보세요." />}
        <DatedList items={lessons}>{(lesson) => (
          <HistoryItem title={lessonTitle(lesson)} description={lesson.content?.distinction}
            meta={`${lessonStatus(lesson)} · ${formatMessageTime(lesson.createdAt)}`}
            onOpen={() => void open(lesson.id)} openLabel={`${lessonTitle(lesson)} 열기`}
            onDelete={() => remove(lesson.id)} deleteLabel={`${lessonTitle(lesson)} 삭제`}
            disabled={busy || loading} />
        )}</DatedList>
      </PageSection>
    </> : <>
      <BackButton onClick={back} disabled={busy} />
      {error && <p role="alert">{error}</p>}
      {loading && <LoadingHint />}
      {generating(selected) && <section className="quiz-panel" aria-live="polite"><h2>새 비교 묶음을 만들고 있어요</h2><p>상황별 문제와 해설을 준비해요. 다른 화면에 다녀와도 계속 생성됩니다.</p></section>}
      {selected.status === "failed" && <section className="quiz-panel"><p>문제 생성에 실패했어요.</p><button type="button" disabled={busy} onClick={() => void run(async () => { const l = await retryNuanceLesson(selected.id); if (l) remember(l); else setError("다시 시도하지 못했어요."); })}>생성 다시 시도</button></section>}
      {content && selected.status === "done" && <PageSection title={lessonTitle(selected)} description={<><span className="nuance-eyebrow">{content.meaning}</span>{" "}<span>{content.distinction}</span></>}>
        <PageToolbar className="nuance-tabs" label="학습 방식">
          <button type="button" className="ghost" aria-pressed={!practicing} onClick={() => setPracticing(false)}>차이 살펴보기</button>
          <button type="button" className="ghost" aria-pressed={practicing} disabled={busy} onClick={() => selected.state.queue.length ? setPracticing(true) : act("start")}>상황 연습</button>
        </PageToolbar>
        {!practicing ? <>
          <div className="nuance-comparison">{content.words.map((word) => <article className="nuance-word" key={word.word}><h3 lang="en">{word.word}</h3><span className="nuance-tone">{word.tone}</span><p>{word.description}</p><p className="nuance-sentence" lang="en">{word.example}</p><p className="nuance-translation">{word.translation}</p></article>)}</div>
          <p className="nuance-caveat">{content.caveat}</p>
          <button type="button" disabled={busy} onClick={() => selected.state.queue.length ? setPracticing(true) : act("start")}>{selected.state.queue.length ? "이어서 풀기" : "상황에 맞게 골라 보기"}</button>
        </> : question ? <>
          <p className="hint">남은 문맥 {selected.state.queue.length}개 · 상황과 말하는 사람의 의도에 가장 잘 맞는 표현을 골라 주세요.</p>
          {/* A saved attempt clears the draft even if the same missed
              question is immediately repeated. Failed saves retain it. */}
          <NuanceQuestionCard key={`${selected.id}-${question.id}-${selected.state.progress[question.id]?.attempts ?? 0}`} lesson={selected} question={question} busy={busy} onAnswer={(word) => act("answer", { questionId: question.id, selected: word })} />
          {selected.state.feedback && <PageToolbar label="복습 진행">
            <button type="button" disabled={busy} onClick={() => act("next")}>{busy ? "저장 중…" : "다음 문맥"}</button>
            {selected.state.feedback.correct && <button type="button" className="ghost" disabled={busy} onClick={() => act("next", { repeat: true })}>맞혔지만 다시 복습</button>}
          </PageToolbar>}
        </> : <section className="quiz-panel" aria-live="polite"><h3>이 묶음의 문맥을 모두 풀었어요</h3><p>맞힌 문제는 간격을 두고 다시 나와요.</p>{next !== null && <p className="hint">다음 복습: {formatAbsoluteDateTime(next)}</p>}<button type="button" className="ghost" onClick={back}>학습 목록 보기</button></section>}
      </PageSection>}
    </>}
  </LearningPage>;
}
