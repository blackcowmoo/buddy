const paths = [
  { href: "nuance", icon: "M4 8c3-4 5 4 8 0s5 4 8 0M4 16c3-4 5 4 8 0s5 4 8 0", title: "단어 뉘앙스", description: "비슷한 단어, 다른 느낌" },
  { href: "writing", icon: "m15 5 4 4M4 20l5-1L20 8a3 3 0 0 0-4-4L5 15l-1 5Z", title: "오늘의 작문", description: "한 문장씩 영어로 표현해요" },
  { href: "words", icon: "M12 6v15M3 4h5a4 4 0 0 1 4 4 4 4 0 0 1 4-4h5v15h-5a4 4 0 0 0-4 2 4 4 0 0 0-4-2H3V4Z", title: "단어 복습", description: "배운 표현을 오래 기억해요" },
  { href: "article", icon: "M6 3h12v18H6V3ZM9 8h6M9 12h6M9 16h4", title: "오늘의 아티클", description: "읽고 이해하며 표현을 넓혀요" },
  { href: "match", icon: "M3 3h7v7H3V3Zm11 0h7v7h-7V3ZM3 14h7v7H3v-7Zm11 0h7v7h-7v-7Z", title: "단어 매칭 게임", description: "단어와 뜻을 짝지어 봐요" },
  { href: "recordings", icon: "M4 10v4M8 6v12M12 3v18M16 6v12M20 10v4", title: "녹음 목록", description: "내가 말한 영어를 다시 들어요" },
];

export function LearningPaths() {
  return (
    <nav className="learning-paths" aria-labelledby="learning-paths-title">
      <div className="section-heading">
        <h2 id="learning-paths-title">오늘은 무엇을 해 볼까요?</h2>
        <p>마음이 가는 연습부터 골라 보세요.</p>
      </div>
      <div className="learning-path-grid">
        {paths.map(({ href, icon, title, description }) => (
          <a className="learning-path" href={href} key={href}>
            <span className="learning-path-icon" aria-hidden="true"><svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round"><path d={icon} /></svg></span>
            <span><strong>{title}</strong><span className="learning-path-description">{description}</span></span>
            <span className="learning-path-arrow" aria-hidden="true">↗</span>
          </a>
        ))}
      </div>
    </nav>
  );
}
