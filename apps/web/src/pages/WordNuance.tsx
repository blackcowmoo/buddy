import { useState } from "react";
import { SubPageHeader } from "../components/SubPageHeader";
import { loadNuanceAnswers, nuanceLessons, saveNuanceAnswers, type NuanceAnswers, type NuanceLesson, type NuanceQuestion } from "../lib/nuance";
import { shuffled } from "../lib/shuffle";

function Question({ lesson, question, answer, onAnswer }: {
  lesson: NuanceLesson;
  question: NuanceQuestion;
  answer: string | undefined;
  onAnswer: (word: string) => void;
}) {
  const [options] = useState(() => shuffled(lesson.words));
  return (
    <section className="nuance-question" aria-labelledby={question.id}>
      <h3 id={question.id}>{question.context}</h3>
      <p className="nuance-sentence" lang="en">{question.sentence}</p>
      <div className="nuance-options" role="group" aria-label="표현 선택">
        {options.map(({ word }) => (
          <button key={word} type="button" className="ghost" disabled={answer !== undefined}
            aria-pressed={answer === word} onClick={() => onAnswer(word)} lang="en">
            {word}
          </button>
        ))}
      </div>
      {answer !== undefined && (
        <div className="nuance-feedback" role="status">
          <strong>{answer === question.answer ? "의도에 맞는 표현이에요" : "이 상황에서는 다른 표현이 더 잘 맞아요"}</strong>
          <p>내 선택: <span lang="en">{answer}</span> · 추천 표현: <b lang="en">{question.answer}</b></p>
          <p className="nuance-sentence" lang="en">{question.sentence.replace("____", question.answer)}</p>
          <p className="nuance-translation">{question.translation}</p>
          <p>{question.explanation}</p>
        </div>
      )}
    </section>
  );
}

export function WordNuance() {
  const [answers, setAnswers] = useState(loadNuanceAnswers);
  const [selected, setSelected] = useState<NuanceLesson | null>(null);
  const [practicing, setPracticing] = useState(false);
  const [search, setSearch] = useState("");
  const [saveFailed, setSaveFailed] = useState(false);
  const [round, setRound] = useState(0);

  function updateAnswers(next: NuanceAnswers) {
    setAnswers(next);
    setSaveFailed(!saveNuanceAnswers(next));
  }

  function retry(onlyMistakes: boolean) {
    if (!selected) return;
    const next = { ...answers };
    for (const question of selected.questions) {
      if (!onlyMistakes || answers[question.id] !== question.answer) delete next[question.id];
    }
    updateAnswers(next);
    setRound((value) => value + 1);
  }

  const query = search.trim().toLocaleLowerCase();
  const lessons = nuanceLessons.filter((lesson) =>
    `${lesson.meaning} ${lesson.words.map(({ word }) => word).join(" ")}`.toLocaleLowerCase().includes(query));
  const correctCount = (lesson: NuanceLesson) => lesson.questions.filter((q) => answers[q.id] === q.answer).length;
  const answeredCount = (lesson: NuanceLesson) => lesson.questions.filter((q) => answers[q.id] !== undefined).length;
  const totalCorrect = nuanceLessons.reduce((total, lesson) => total + correctCount(lesson), 0);
  const totalQuestions = nuanceLessons.reduce((total, lesson) => total + lesson.questions.length, 0);

  return (
    <div className="app">
      <SubPageHeader title="단어 뉘앙스" />
      <main className="convo nuance-page">
        <div className="nuance-intro">
          <p className="nuance-eyebrow">같은 한국어, 다른 느낌</p>
          <h2>뜻을 넘어, 상황에 맞는 단어로</h2>
          <p>한국어 뜻이 겹치는 영어 표현을 비교하고, 전달하려는 느낌에 맞게 골라 보세요.</p>
          <p className="nuance-progress">{nuanceLessons.length}개 묶음 · 맞힌 문맥 {totalCorrect}/{totalQuestions}</p>
        </div>
        {saveFailed && <p role="alert">학습 진행을 저장하지 못했어요. 이 화면에서는 계속 풀 수 있지만, 새로고침하면 최근 답변이 사라질 수 있어요.</p>}
        {selected ? (
          <>
            <button type="button" className="ghost nuance-list-back" onClick={() => setSelected(null)}>← 비교 목록</button>
            <div className="nuance-lesson-heading">
              <p className="nuance-eyebrow">{selected.meaning}</p>
              <h2 lang="en">{selected.words.map(({ word }) => word).join(" / ")}</h2>
              <p>{selected.distinction}</p>
            </div>
            <div className="nuance-tabs" role="group" aria-label="학습 방식">
              <button type="button" className="ghost" aria-pressed={!practicing} onClick={() => setPracticing(false)}>차이 살펴보기</button>
              <button type="button" className="ghost" aria-pressed={practicing} onClick={() => setPracticing(true)}>상황 연습</button>
            </div>
            {practicing ? (
              <>
                <p className="hint">문법상 가능한 단어가 여러 개일 수도 있어요. 설명된 상황과 말하는 사람의 의도에 가장 잘 맞는 표현을 골라 주세요.</p>
                {selected.questions.map((question) => (
                  <Question key={`${question.id}-${round}`} lesson={selected} question={question} answer={answers[question.id]}
                    onAnswer={(word) => updateAnswers({ ...answers, [question.id]: word })} />
                ))}
                <div className="nuance-review">
                  <p>풀이 {answeredCount(selected)}/{selected.questions.length} · 맞힌 문맥 {correctCount(selected)}/{selected.questions.length}</p>
                  {answeredCount(selected) > correctCount(selected) && (
                    <button type="button" className="ghost" onClick={() => retry(true)}>틀린 문제 다시 풀기</button>
                  )}
                  {answeredCount(selected) === selected.questions.length && (
                    <button type="button" className="ghost" onClick={() => retry(false)}>이 묶음 다시 풀기</button>
                  )}
                </div>
              </>
            ) : (
              <>
                <div className="nuance-comparison">
                  {selected.words.map((word) => (
                    <article className="nuance-word" key={word.word}>
                      <h3 lang="en">{word.word}</h3>
                      <span className="nuance-tone">{word.tone}</span>
                      <p>{word.description}</p>
                      <p className="nuance-sentence" lang="en">{word.example}</p>
                      <p className="nuance-translation">{word.translation}</p>
                      <a href={`https://dictionary.cambridge.org/dictionary/english/${word.word}`} target="_blank" rel="noreferrer">{word.word} 사전 보기 ↗</a>
                    </article>
                  ))}
                </div>
                <p className="nuance-caveat">{selected.caveat}</p>
                <button type="button" onClick={() => setPracticing(true)}>상황에 맞게 골라 보기</button>
              </>
            )}
          </>
        ) : (
          <>
            <label className="nuance-search">비교 묶음 찾기
              <input type="search" value={search} onChange={(e) => setSearch(e.target.value)} placeholder="한국어 뜻 또는 영어 단어" />
            </label>
            <div className="nuance-lessons">
              {lessons.map((lesson) => (
                <button type="button" className="ghost nuance-lesson" key={lesson.id} onClick={() => { setSelected(lesson); setPracticing(false); }}>
                  <span className="nuance-eyebrow">{lesson.meaning}</span>
                  <strong lang="en">{lesson.words.map(({ word }) => word).join(" / ")}</strong>
                  <span>{lesson.distinction}</span>
                  <span className="nuance-progress">{answeredCount(lesson) === 0 ? "시작하기 →" : `풀이 ${answeredCount(lesson)}/${lesson.questions.length} · 맞힌 문맥 ${correctCount(lesson)}/${lesson.questions.length}`}</span>
                </button>
              ))}
            </div>
            {lessons.length === 0 && <p role="status">일치하는 비교 묶음이 없어요. 다른 한국어 뜻이나 영어 단어로 찾아보세요.</p>}
          </>
        )}
        <p className="hint nuance-storage-note">진행은 이 브라우저에 저장돼요. 다른 기기와는 동기화되지 않아요.</p>
      </main>
    </div>
  );
}
