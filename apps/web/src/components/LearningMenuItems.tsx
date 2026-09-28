const learningMenuItems = [
  { href: "recordings", label: "녹음 목록", icon: "🎧" },
  { href: "instant", label: "인스턴트 대화 목록", icon: "⚡" },
  { href: "words", label: "단어 복습", icon: "📚" },
  { href: "match", label: "단어 매칭 게임", icon: "🎮" },
  { href: "nuance", label: "단어 뉘앙스", icon: "🪄" },
  { href: "article", label: "오늘의 아티클", icon: "📰" },
  { href: "writing", label: "오늘의 작문", icon: "✍️" },
];

export function LearningMenuItems({
  wordDueCount = 0,
  nuanceDueCount = 0,
}: {
  wordDueCount?: number;
  nuanceDueCount?: number;
}) {
  return (
    <>
      {learningMenuItems.map((item) => {
        const dueCount = item.href === "words" ? wordDueCount : item.href === "nuance" ? nuanceDueCount : 0;
        return (
          <a key={item.href} className="ghost menu-item" href={item.href} role="menuitem">
            <span aria-hidden="true">{item.icon}</span> {item.label}
            {dueCount > 0 && <span className="menu-badge">{dueCount}</span>}
          </a>
        );
      })}
    </>
  );
}
