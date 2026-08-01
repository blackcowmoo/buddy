// Fisher-Yates shuffle, in place on a copy so the caller's array is never
// mutated. Shared by WordReview.tsx (quiz question order/choices) and
// WordMatch.tsx (the matching game's card layout).
export function shuffled<T>(items: T[]): T[] {
  const out = [...items];
  for (let i = out.length - 1; i > 0; i--) {
    const j = Math.floor(Math.random() * (i + 1));
    [out[i], out[j]] = [out[j], out[i]];
  }
  return out;
}
