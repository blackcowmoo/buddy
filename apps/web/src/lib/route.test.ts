import { describe, expect, it } from "vitest";
import { currentPage } from "./route";

describe("currentPage", () => {
  it("treats root as the chat page", () => {
    expect(currentPage("/")).toBe("chat");
  });

  it("treats an unrelated path as the chat page", () => {
    expect(currentPage("/pr/14/")).toBe("chat");
  });

  it("recognizes the recordings page at root", () => {
    expect(currentPage("/recordings")).toBe("recordings");
  });

  it("recognizes the recordings page under a ROOT_PATH prefix", () => {
    expect(currentPage("/pr/14/recordings")).toBe("recordings");
  });

  it("tolerates a trailing slash", () => {
    expect(currentPage("/recordings/")).toBe("recordings");
  });

  it("recognizes the word-review page at root", () => {
    expect(currentPage("/words")).toBe("words");
  });

  it("recognizes the word-review page under a ROOT_PATH prefix", () => {
    expect(currentPage("/pr/14/words")).toBe("words");
  });

  it("recognizes the word-match game page at root", () => {
    expect(currentPage("/match")).toBe("match");
  });

  it("recognizes the word-match game page under a ROOT_PATH prefix", () => {
    expect(currentPage("/pr/14/match")).toBe("match");
  });

  it("recognizes the instant-conversations page at root", () => {
    expect(currentPage("/instant")).toBe("instant");
  });

  it("recognizes the instant-conversations page under a ROOT_PATH prefix", () => {
    expect(currentPage("/pr/14/instant")).toBe("instant");
  });

  it("recognizes the article-quiz page at root", () => {
    expect(currentPage("/article")).toBe("article");
  });

  it("recognizes the article-quiz page under a ROOT_PATH prefix", () => {
    expect(currentPage("/pr/14/article")).toBe("article");
  });
});
