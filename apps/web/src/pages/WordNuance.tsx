import { Fragment, useCallback, useEffect, useRef, useState } from "react";
import { SubPageHeader } from "../components/SubPageHeader";
import { LoadingHint } from "../components/LoadingHint";
import { deleteNuanceLesson, drawNuanceLesson, dueQuestions, fetchNuanceLesson, fetchNuanceLessons, lessonTitle, nextReview, practiceNuance, retryNuanceLesson, nuanceOptions, type NuanceAction, type NuanceLesson, type NuanceQuestion } from "../lib/nuance";
import { formatAbsoluteDateTime, formatDateDivider, formatMessageTime, shouldShowDateDivider } from "../lib/time";

function Question({ lesson, question, busy, onAnswer }: {
  lesson: NuanceLesson; question: NuanceQuestion; busy: boolean; onAnswer: (word: string) => void;
}) {
  const attempt = (lesson.state.progress[question.id]?.attempts ?? 0) - (lesson.state.feedback ? 1 : 0);
  const options = nuanceOptions(lesson, question.id, attempt);
  const feedback = lesson.state.feedback;
  const reviewed = lesson.state.progress[question.id]?.lastReviewedAt;
  return <section className="nuance-question" aria-labelledby={`question-${question.id}`}>
    <h3 id={`question-${question.id}`}>{question.context}</h3>
    {!feedback && <p className="hint">{reviewed ? `지난 풀이: ${formatAbsoluteDateTime(reviewed)}` : "처음 만나는 문맥이에요"}</p>}
    <p className="nuance-sentence" lang="en">{question.sentence}</p>
    <div className="nuance-options" role="group" aria-label="표현 선택">
      {options.map(({ word }) => <button key={word} type="button" className="ghost" disabled={busy || !!feedback}
        aria-pressed={feedback?.selected === word} onClick={() => onAnswer(word)} lang="en">{word}</button>)}
    </div>
    {feedback && <div className="nuance-feedback" role="status">
      <strong>{feedback.correct ? "의도에 맞는 표현이에요" : "이 상황에서는 다른 표현이 더 잘 맞아요"}</strong>
      <p>내 선택: <span lang="en">{feedback.selected}</span> · 추천 표현: <b lang="en">{question.answer}</b></p>
      <p className="nuance-sentence" lang="en">{question.sentence.replace("____", question.answer)}</p>
      <p className="nuance-translation">{question.translation}</p>
      <p>{question.explanation}</p>
      {!feedback.correct && <p>이 문제는 잠시 뒤 다시 나와요.</p>}
    </div>}
  </section>;
}

const selectedID = () => new URLSearchParams(window.location.search).get("lesson");
function navigate(id: string | null) {
  const url = new URL(window.location.href);
  if (id) url.searchParams.set("lesson", id); else url.searchParams.delete("lesson");
  window.history.pushState(null, "", url);
}
const generating = (l: NuanceLesson) => l.status === "pending" || l.status === "processing";

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
    setLessons((items) => items.some((l) => l.id === lesson.id) ? items.map((l) => l.id === lesson.id && l.revision <= lesson.revision ? lesson : l) : [...items, lesson]);
    setSelected((old) => old?.id === lesson.id && old.revision <= lesson.revision ? lesson : old);
  }, []);

  const load = useCallback(async () => {
    setLoading(true);
    const items = await fetchNuanceLessons();
    if (items) { setLessons(items); setError(null); }
    else setError("학습 목록을 불러오지 못했어요. 다시 시도해 주세요.");
    setLoading(false);
  }, []);

  const open = useCallback(async (id: string, push = true, review = false) => {
    const token = ++navigation.current;
    setLoading(true); setError(null);
    let lesson = await fetchNuanceLesson(id);
    if (token !== navigation.current) return;
    if (review && lesson?.status === "done" && !lesson.state.queue.length) {
      lesson = await practiceNuance(id, { kind: "start", revision: lesson.revision });
    }
    if (token !== navigation.current) return;
    setLoading(false);
    if (!lesson) { setError("문제를 불러오지 못했어요. 목록에서 다시 열어 주세요."); return; }
    remember(lesson); setSelected(lesson); setPracticing(review || lesson.state.queue.length > 0);
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
      if (items) { setLessons(items); setSelected((old) => old ? items.find((l) => l.id === old.id && l.revision >= old.revision) ?? old : null); }
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
  const reviewLesson = lessons.find((l) => l.state.queue.length > 0 || dueQuestions(l) > 0);
  const content = selected?.content;
  const question = content?.questions.find((q) => q.id === selected?.state.queue[0]);
  const next = selected ? nextReview(selected) : null;

  return <div className="app">
    <SubPageHeader title="단어 뉘앙스" />
    <main className="convo nuance-page">
      {error && <p role="alert">{error}</p>}
      {loading && <LoadingHint />}
      {!selected ? <>
        <div className="nuance-intro">
          <p className="nuance-eyebrow">같은 뜻, 미묘하게 다른 느낌</p>
          <h2>상황에 맞는 단어를 익혀요</h2>
          <p>서로 바꿔 써도 기본 뜻이 통하는 단어들을 비교해요. 상황과 의도에 따라 달라지는 말투와 느낌을 익혀 보세요.</p>
          <p className="nuance-progress">{lessons.length}개 묶음 · 복습할 문맥 {due}개</p>
        </div>
        <div className="nuance-review">
          <button type="button" onClick={create} disabled={busy || loading}>{busy ? "처리 중…" : "＋ 새 문제 만들기"}</button>
          {reviewLesson && <button type="button" className="ghost" disabled={busy || loading} onClick={() => void open(reviewLesson.id, true, true)}>복습 시작</button>}
        </div>
        {!loading && error && <button type="button" className="ghost" onClick={() => void load()}>목록 다시 불러오기</button>}
        {!loading && !error && lessons.length === 0 && <p className="hint">아직 만든 문제가 없어요. 첫 비교 묶음을 만들어 보세요.</p>}
        {lessons.map((lesson, i) => <Fragment key={lesson.id}>
          {shouldShowDateDivider(lessons[i - 1]?.createdAt, lesson.createdAt) && <div className="date-divider"><span>{formatDateDivider(lesson.createdAt)}</span></div>}
          <div className="session-row">
            <button type="button" className="session-item" aria-label={`${lessonTitle(lesson)} 열기`} disabled={busy || loading} onClick={() => void open(lesson.id)}>
              <span className="title">{lessonTitle(lesson)}</span>
              {lesson.content && <span className="nuance-list-description">{lesson.content.distinction}</span>}
              <span className="time writing-list-meta">
                {generating(lesson) ? "생성 중" : lesson.status === "failed" ? "생성 실패 · 다시 시도" : lesson.state.queue.length ? `학습 중 · 남은 문맥 ${lesson.state.queue.length}개` : dueQuestions(lesson) ? `복습할 문맥 ${dueQuestions(lesson)}개` : `복습 완료 · 다음 ${formatAbsoluteDateTime(nextReview(lesson)!)}`}
                {" · "}{formatMessageTime(lesson.createdAt)}
              </span>
            </button>
            <button type="button" className="ghost icon-btn session-delete" disabled={busy || loading} onClick={() => remove(lesson.id)} aria-label={`${lessonTitle(lesson)} 삭제`}>🗑</button>
          </div>
        </Fragment>)}
      </> : <>
        <button type="button" className="ghost nuance-list-back" onClick={back} disabled={busy}>← 목록으로</button>
        {generating(selected) && <section className="quiz-panel" aria-live="polite"><h2>새 비교 묶음을 만들고 있어요</h2><p>상황별 문제와 해설을 준비해요. 다른 화면에 다녀와도 계속 생성됩니다.</p></section>}
        {selected.status === "failed" && <section className="quiz-panel"><p>문제 생성에 실패했어요.</p><button type="button" disabled={busy} onClick={() => void run(async () => { const l = await retryNuanceLesson(selected.id); if (l) remember(l); else setError("다시 시도하지 못했어요."); })}>생성 다시 시도</button></section>}
        {content && selected.status === "done" && <>
          <div className="nuance-lesson-heading"><p className="nuance-eyebrow">{content.meaning}</p><h2 lang="en">{lessonTitle(selected)}</h2><p>{content.distinction}</p></div>
          <div className="nuance-tabs" role="group" aria-label="학습 방식">
            <button type="button" className="ghost" aria-pressed={!practicing} onClick={() => setPracticing(false)}>차이 살펴보기</button>
            <button type="button" className="ghost" aria-pressed={practicing} disabled={busy} onClick={() => selected.state.queue.length ? setPracticing(true) : act("start")}>상황 연습</button>
          </div>
          {!practicing ? <>
            <div className="nuance-comparison">{content.words.map((word) => <article className="nuance-word" key={word.word}><h3 lang="en">{word.word}</h3><span className="nuance-tone">{word.tone}</span><p>{word.description}</p><p className="nuance-sentence" lang="en">{word.example}</p><p className="nuance-translation">{word.translation}</p></article>)}</div>
            <p className="nuance-caveat">{content.caveat}</p>
            <button type="button" disabled={busy} onClick={() => selected.state.queue.length ? setPracticing(true) : act("start")}>{selected.state.queue.length ? "이어서 풀기" : "상황에 맞게 골라 보기"}</button>
          </> : question ? <>
            <p className="hint">남은 문맥 {selected.state.queue.length}개 · 상황과 말하는 사람의 의도에 가장 잘 맞는 표현을 골라 주세요.</p>
            <Question key={`${selected.id}-${question.id}`} lesson={selected} question={question} busy={busy} onAnswer={(word) => act("answer", { questionId: question.id, selected: word })} />
            {selected.state.feedback && <div className="nuance-review">
              <button type="button" disabled={busy} onClick={() => act("next")}>{busy ? "저장 중…" : "다음 문맥"}</button>
              {selected.state.feedback.correct && <button type="button" className="ghost" disabled={busy} onClick={() => act("next", { repeat: true })}>맞혔지만 다시 복습</button>}
            </div>}
          </> : <section className="quiz-panel" aria-live="polite"><h3>지금 복습할 문맥을 모두 풀었어요</h3><p>맞힌 문제는 간격을 두고 다시 나와요.</p>{next !== null && <p className="hint">다음 복습: {formatAbsoluteDateTime(next)}</p>}{reviewLesson && reviewLesson.id !== selected.id && <button type="button" onClick={() => void open(reviewLesson.id, true, true)}>다음 묶음 복습하기</button>}<button type="button" className="ghost" onClick={back}>학습 목록 보기</button></section>}
        </>}
      </>}
      <p className="hint nuance-storage-note">문제와 풀이 진행은 계정에 저장돼요. 의도에 더 잘 맞는 표현을 놓친 문맥은 다시 풀고, 익숙해진 문맥은 복습 간격이 늘어나요.</p>
    </main>
  </div>;
}
