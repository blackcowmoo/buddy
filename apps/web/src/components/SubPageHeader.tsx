// Shared top bar for every sub-page reached from the room list (Recordings,
// WordReview, WordMatch, InstantSessions, ArticleQuiz, Writing): a title plus a
// "back to chat" link. The link is relative ("." not "/") so it resolves
// against the current page URL and still works under a ROOT_PATH prefix
// like "/pr/14/words".
export function SubPageHeader({ title }: { title: string }) {
  return (
    <header className="topbar">
      <div className="brand">
        <h1>{title}</h1>
      </div>
      <a className="ghost icon-btn" href="." aria-label="대화로 돌아가기" title="대화로 돌아가기">
        ←
      </a>
    </header>
  );
}
