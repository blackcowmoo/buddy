/** @vitest-environment jsdom */
import "@testing-library/jest-dom/vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, expect, it } from "vitest";
import { LearningIntro } from "./LearningIntro";
import { LoadingHint } from "./LoadingHint";

afterEach(cleanup);

it("associates each introduction with its own heading and presents steps in order", () => {
  render(<>
    <LearningIntro eyebrow="한 문장씩" title="작문 연습" description="편하게 써 보세요." steps={["문제 만들기", "영어로 쓰기", "피드백 살펴보기"]} />
    <LearningIntro eyebrow="다시 만나기" title="단어 복습" description="천천히 익혀요." />
  </>);
  const writing = screen.getByRole("region", { name: "작문 연습" });
  expect(within(writing).getByText("편하게 써 보세요.")).toBeInTheDocument();
  expect(within(writing).getAllByRole("listitem").map((item) => item.textContent)).toEqual(["문제 만들기", "영어로 쓰기", "피드백 살펴보기"]);
  expect(within(screen.getByRole("region", { name: "단어 복습" })).queryByRole("list")).not.toBeInTheDocument();
});

it("announces shared loading feedback to assistive technology", () => {
  render(<LoadingHint />);
  expect(screen.getByRole("status")).toHaveTextContent("불러오는 중…");
});
