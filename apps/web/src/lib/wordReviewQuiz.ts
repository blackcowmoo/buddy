import { wordStudyStatus, type WordReviewItem, type WordReviewQuestion } from "./wordReview";
import { shuffled } from "./shuffle";

// The server decides which contract version it is upgrading to; a question
// stays servable at any version as long as it is complete and gradeable.
// The server only sends questions it considers solvable (see
// wordreview.QuestionSolvable) and regenerates older-version ones in the
// background, so this only re-validates the shape, never the version number.
const minQuestionVersion = 1;
const minRecognitionDistractors = 7;

export type QuizItem =
  | { word: WordReviewItem; mode: "recall" }
  | { word: WordReviewItem; mode: "recognition"; choices: string[] };

// A question can be shown and submitted whenever every "___" blank has one
// non-blank answer. An older-version question stays answerable while the
// server upgrades it; its stored version is sent back with the answer so the
// server can bind the attempt to the question the learner actually saw.
export function currentQuestion(word: WordReviewItem): WordReviewQuestion | null {
  const question = word.reviewQuestion;
  if (!question || question.version < minQuestionVersion) return null;
  if (!Array.isArray(question.answers)) return null;
  if (question.answers.length === 0 || question.prompt.split("___").length !== question.answers.length + 1) return null;
  if (question.answers.some((answer) => !answer.trim())) return null;
  return question;
}

// Build once when review starts: later polling must not reorder the quiz.
// Recognition uses other confirmed meanings already loaded in the browser;
// smaller vocabularies use the stored recall question without waiting on an LLM.
export function buildReviewQueue(words: readonly WordReviewItem[], now: number): QuizItem[] {
  const confirmed = words.filter((word) => wordStudyStatus(word) === "confirmed");
  const due = confirmed.filter((word) => word.nextReviewAt <= now && currentQuestion(word) !== null);
  return shuffled(due).map((word) => {
    const otherMeanings = confirmed.filter((other) => other.id !== word.id).map((other) => other.meaning);
    if (otherMeanings.length < minRecognitionDistractors || Math.random() >= 0.5) {
      return { word, mode: "recall" };
    }
    const distractors = shuffled(otherMeanings).slice(0, minRecognitionDistractors);
    return { word, mode: "recognition", choices: shuffled([word.meaning, ...distractors]) };
  });
}

// A missed recognition question gets a new choice order on retry. The swap
// guarantees movement even when the shuffle happens to return the old order.
export function reshuffleRecognitionChoices(item: QuizItem): QuizItem {
  if (item.mode !== "recognition" || item.choices.length < 2) return item;
  const choices = shuffled(item.choices);
  if (choices.every((choice, index) => choice === item.choices[index])) {
    [choices[0], choices[1]] = [choices[1], choices[0]];
  }
  return { ...item, choices };
}
