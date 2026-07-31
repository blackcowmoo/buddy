// Asks the server (httpserver.quizAnswerCheckHandler ->
// pipeline.CheckQuizAnswer) whether a learner's typed quiz answer should
// count as correct, for the one case QuizPanel's own isQuizAnswerAccepted
// can't already resolve: an answer that didn't literally match
// QuizQuestion.answer or acceptableAnswers, but might still be a genuine
// synonym the model didn't think to list at quiz-generation time. Returns
// false — not null/throwing — on any failure (network error, non-200, bad
// JSON) as well as an explicit "no": an unconfirmed answer must grade wrong,
// the same bias quizAnswerCheckSystemPrompt applies server-side, so a flaky
// request can't accidentally grade a genuinely wrong answer as right.
export async function checkQuizAnswer(
  prompt: string,
  answer: string,
  acceptableAnswers: string[] | undefined,
  learnerAnswer: string,
): Promise<boolean> {
  try {
    const res = await fetch("api/quiz/check-answer", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ prompt, answer, acceptableAnswers, learnerAnswer }),
    });
    if (!res.ok) return false;
    const body = (await res.json()) as { correct: boolean };
    return body.correct === true;
  } catch {
    return false;
  }
}
