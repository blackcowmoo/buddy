import { useState } from "react";
import { nuanceOptions, type NuanceLesson, type NuanceQuestion } from "../lib/nuance";
import { formatAbsoluteDateTime } from "../lib/time";
import { QuizChoices } from "./QuizChoices";

export function NuanceQuestionCard({ lesson, question, busy, onAnswer }: {
  lesson: NuanceLesson; question: NuanceQuestion; busy: boolean; onAnswer: (word: string) => void;
}) {
  const [selection, setSelection] = useState<string | null>(null);
  const attempt = (lesson.state.progress[question.id]?.attempts ?? 0) - (lesson.state.feedback ? 1 : 0);
  const options = nuanceOptions(lesson, question.id, attempt);
  const feedback = lesson.state.feedback;
  const reviewed = lesson.state.progress[question.id]?.lastReviewedAt;
  return <section className="nuance-question" aria-labelledby={`question-${lesson.id}-${question.id}`}>
    <h3 id={`question-${lesson.id}-${question.id}`}>{question.context}</h3>
    {!feedback && <p className="hint">{reviewed ? `지난 풀이: ${formatAbsoluteDateTime(reviewed)}` : "처음 만나는 문맥이에요"}</p>}
    <p className="nuance-sentence" lang="en">{question.sentence}</p>
    <QuizChoices
      options={options.map(({ word }) => word)}
      selectedIndex={options.findIndex(({ word }) => word === (feedback?.selected ?? selection))}
      correctIndex={feedback ? options.findIndex(({ word }) => word === question.answer) : undefined}
      disabled={busy}
      label="표현 선택"
      lang="en"
      onSelect={(index) => setSelection(options[index].word)}
    />
    {!feedback && <button type="button" className="quiz-check-btn" disabled={busy || selection === null}
      onClick={() => { if (selection !== null) onAnswer(selection); }}>{busy ? "채점 중…" : "답안 확인"}</button>}
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
