import { describe, expect, it } from "vitest";
import { createConversationEndState } from "./conversationEnd";

describe("createConversationEndState", () => {
  it("provides the active-conversation defaults", () => {
    expect(createConversationEndState()).toEqual({
      ended: false,
      studySummary: [],
      studySummaryStatus: "done",
      quiz: [],
      quizStatus: "done",
      quizCompleted: false,
    });
  });

  it("normalizes optional API fields while preserving completed work", () => {
    const studySummary = [{ english: "I went home.", translation: "나는 집에 갔다." }];
    const quiz = [{
      prompt: "I ___ home.",
      answer: "went",
      translation: "나는 집에 갔다.",
      explanation: "The action happened in the past.",
      explanationTranslation: "그 행동은 과거에 일어났다.",
    }];

    expect(createConversationEndState({
      id: "s1",
      title: "Session",
      createdAt: 1,
      updatedAt: 2,
      ended: true,
      studySummary,
      studySummaryStatus: "pending",
      quiz,
      quizStatus: "failed",
      quizCompleted: true,
    })).toEqual({
      ended: true,
      studySummary,
      studySummaryStatus: "pending",
      quiz,
      quizStatus: "failed",
      quizCompleted: true,
    });
  });
});
