export type LearningMenuKey =
  | "recordings"
  | "instant"
  | "words"
  | "match"
  | "nuance"
  | "article"
  | "writing";

const learningMenuItems: Array<{ key: LearningMenuKey; href: string; label: string; icon: string }> = [
  { key: "recordings", href: "recordings", label: "녹음 목록", icon: "🎧" },
  { key: "instant", href: "instant", label: "인스턴트 대화 목록", icon: "⚡" },
  { key: "words", href: "words", label: "단어 복습", icon: "📚" },
  { key: "match", href: "match", label: "단어 매칭 게임", icon: "🎮" },
  { key: "nuance", href: "nuance", label: "단어 뉘앙스", icon: "🪄" },
  { key: "article", href: "article", label: "오늘의 아티클", icon: "📰" },
  { key: "writing", href: "writing", label: "오늘의 작문", icon: "✍️" },
];

export function LearningMenuItems({
  onSelect,
  wordDueCount = 0,
  nuanceDueCount = 0,
}: {
  onSelect?: (key: LearningMenuKey) => void;
  wordDueCount?: number;
  nuanceDueCount?: number;
}) {
  return (
    <>
      {learningMenuItems.map((item) => {
        const dueCount = item.key === "words" ? wordDueCount : item.key === "nuance" ? nuanceDueCount : 0;
        const content = <><span aria-hidden="true">{item.icon}</span> {item.label}{dueCount > 0 && <span className="menu-badge">{dueCount}</span>}</>;
        if (onSelect) {
          return <button key={item.key} type="button" className="ghost menu-item" onClick={() => onSelect(item.key)} role="menuitem">{content}</button>;
        }
        return <a key={item.key} className="ghost menu-item" href={item.href} role="menuitem">{content}</a>;
      })}
    </>
  );
}
