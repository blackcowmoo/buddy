import type { Page } from "./route";

export type LearningDestinationId = Exclude<Page, "chat" | "nuance-review">;

export interface LearningDestination {
  href: LearningDestinationId;
  label: string;
  description: string;
  icon: string;
}

// Keep the overview and every page's menu in the same order, with the same
// names and icons, so adding a destination does not create a separate flow.
export const learningDestinations = [
  { href: "recordings", label: "녹음 목록", description: "내가 말한 영어를 다시 들어요", icon: "M4 10v4M8 6v12M12 3v18M16 6v12M20 10v4" },
  { href: "instant", label: "인스턴트 대화 목록", description: "짧게 나눈 대화를 이어 봐요", icon: "m13 2-9 12h7l-1 8 10-12h-7l1-8Z" },
  { href: "words", label: "단어 복습", description: "배운 표현을 오래 기억해요", icon: "M12 6v15M3 4h5a4 4 0 0 1 4 4 4 4 0 0 1 4-4h5v15h-5a4 4 0 0 0-4 2 4 4 0 0 0-4-2H3V4Z" },
  { href: "match", label: "단어 매칭 게임", description: "단어와 뜻을 짝지어 봐요", icon: "M3 3h7v7H3V3Zm11 0h7v7h-7V3ZM3 14h7v7H3v-7Zm11 0h7v7h-7v-7Z" },
  { href: "nuance", label: "단어 뉘앙스", description: "비슷한 단어, 다른 느낌", icon: "M4 8c3-4 5 4 8 0s5 4 8 0M4 16c3-4 5 4 8 0s5 4 8 0" },
  { href: "article", label: "오늘의 아티클", description: "읽고 이해하며 표현을 넓혀요", icon: "M6 3h12v18H6V3ZM9 8h6M9 12h6M9 16h4" },
  { href: "writing", label: "오늘의 작문", description: "한 문장씩 영어로 표현해요", icon: "m15 5 4 4M4 20l5-1L20 8a3 3 0 0 0-4-4L5 15l-1 5Z" },
] as const satisfies readonly LearningDestination[];

export function learningDestinationForPage(page: Page): LearningDestination | undefined {
  const destination = page === "nuance-review" ? "nuance" : page;
  return learningDestinations.find((item) => item.href === destination);
}
