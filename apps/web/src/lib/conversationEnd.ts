import type { QuizQuestion, StudySummarySentence } from "./protocol";
import type { SessionJobStatus, SessionSummary } from "./sessions";

export interface ConversationEndState {
  ended: boolean;
  studySummary: StudySummarySentence[];
  studySummaryStatus: SessionJobStatus;
  quiz: QuizQuestion[];
  quizStatus: SessionJobStatus;
  quizCompleted: boolean;
}

export function createConversationEndState(session?: SessionSummary): ConversationEndState {
  return {
    ended: session?.ended ?? false,
    studySummary: session?.studySummary ?? [],
    studySummaryStatus: session?.studySummaryStatus || "done",
    quiz: session?.quiz ?? [],
    quizStatus: session?.quizStatus || "done",
    quizCompleted: session?.quizCompleted ?? false,
  };
}
