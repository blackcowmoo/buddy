import { describe, expect, it } from "vitest";
import type { TurnRecord } from "./sessions";
import { hydrateTurnPage } from "./turns";

describe("hydrateTurnPage", () => {
  it("drops pending reply placeholders and reports that polling is needed", () => {
    const turns: TurnRecord[] = [
      { turn: 3, role: "assistant", text: "", refined: false, replyStatus: "processing" },
    ];

    expect(hydrateTurnPage(turns)).toEqual({
      messages: [],
      metadata: {},
      pending: true,
      awaitingReply: true,
    });
  });

  it("maps messages and merges user and assistant metadata sharing a turn", () => {
    const turns: TurnRecord[] = [
      {
        turn: 1,
        role: "user",
        text: "I go home.",
        refined: true,
        source: "voice",
        createdAt: 10,
        correction: {
          original: "I go home.",
          corrected: "I went home.",
          translation: "나는 집에 갔다.",
          issues: [],
        },
        correctionUnread: true,
      },
      {
        turn: 1,
        role: "assistant",
        text: "Sounds good.",
        refined: false,
        translation: "좋아요.",
        createdAt: 11,
      },
    ];

    expect(hydrateTurnPage(turns)).toEqual({
      messages: [
        { turn: 1, role: "user", text: "I go home.", refined: true, source: "voice", timestamp: 10 },
        { turn: 1, role: "assistant", text: "Sounds good.", refined: false, source: undefined, timestamp: 11 },
      ],
      metadata: {
        1: {
          correction: turns[0].correction,
          correctionUnread: true,
          userTranslation: "나는 집에 갔다.",
          assistantTranslation: "좋아요.",
        },
      },
      pending: false,
      awaitingReply: false,
    });
  });

  it("marks a recent untracked user turn and missing translations as pending", () => {
    const turns: TurnRecord[] = [
      { turn: 2, role: "user", text: "Hello", refined: false },
      { turn: 2, role: "assistant", text: "Hi", refined: false },
    ];

    expect(hydrateTurnPage(turns, true)).toMatchObject({
      metadata: {
        2: {
          correctionPending: true,
          userTranslationPending: true,
          assistantTranslationPending: true,
        },
      },
      pending: true,
      awaitingReply: false,
    });
  });
});
