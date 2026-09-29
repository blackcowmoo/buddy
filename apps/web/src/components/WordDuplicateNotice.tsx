import { wordStudyStatus, type WordReviewItem } from "../lib/wordReview";

export function WordDuplicateNotice({ words }: { words: WordReviewItem[] }) {
  if (words.length === 0) return null;
  return (
    <div className="word-duplicate-notice" role="note" aria-label="같은 영어 단어 안내">
      <strong>같은 영어 단어가 있어요</strong>
      {words.map((word) => (
        <span key={word.id}>
          {wordStudyStatus(word) === "confirmed" ? "학습 중" : "확정 전"}
          {" · "}{word.meaning}
        </span>
      ))}
    </div>
  );
}
