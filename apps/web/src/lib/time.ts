// Korean-locale timestamp formatting shared by the chat view (App.tsx) and
// the recordings list (pages/Recordings.tsx), so a moment reads the same way
// everywhere in the app instead of each screen picking its own convention
// (e.g. browser-default toLocaleString()).
const WEEKDAYS_KO = ["일", "월", "화", "수", "목", "금", "토"];

export function formatRelativeTime(unixSeconds: number): string {
  const mins = Math.floor((Date.now() - unixSeconds * 1000) / 60000);
  if (mins < 1) return "방금 전";
  if (mins < 60) return `${mins}분 전`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return `${hours}시간 전`;
  const days = Math.floor(hours / 24);
  if (days < 30) return `${days}일 전`;
  return new Date(unixSeconds * 1000).toLocaleDateString();
}

// Local calendar day the two timestamps fall on — not a 24h-window diff, so
// 11:59pm and 12:01am on consecutive days count as different days even
// though they're 2 minutes apart.
export function isSameDay(aUnixSeconds: number, bUnixSeconds: number): boolean {
  const a = new Date(aUnixSeconds * 1000);
  const b = new Date(bUnixSeconds * 1000);
  return (
    a.getFullYear() === b.getFullYear() &&
    a.getMonth() === b.getMonth() &&
    a.getDate() === b.getDate()
  );
}

// Divider label shown between messages sent on different days: "오늘"/"어제"
// for the last two days, "7월 20일 (월)" within the current year, and a full
// "2024. 05. 20. (화)" once the year rolls over — each step drops precision
// that's no longer useful (nobody needs the year for something said today).
export function formatDateDivider(unixSeconds: number): string {
  const d = new Date(unixSeconds * 1000);
  const now = new Date();
  const startOfDay = (x: Date) => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime();
  const diffDays = Math.round((startOfDay(now) - startOfDay(d)) / 86_400_000);
  if (diffDays === 0) return "오늘";
  if (diffDays === 1) return "어제";
  const weekday = WEEKDAYS_KO[d.getDay()];
  if (d.getFullYear() === now.getFullYear()) return `${d.getMonth() + 1}월 ${d.getDate()}일 (${weekday})`;
  return formatAbsoluteDate(d);
}

// Per-message clock time, Korean AM/PM convention ("오전/오후 h:mm").
export function formatMessageTime(unixSeconds: number): string {
  const d = new Date(unixSeconds * 1000);
  const period = d.getHours() < 12 ? "오전" : "오후";
  const h12 = d.getHours() % 12 || 12;
  const mm = String(d.getMinutes()).padStart(2, "0");
  return `${period} ${h12}:${mm}`;
}

function formatAbsoluteDate(d: Date): string {
  const weekday = WEEKDAYS_KO[d.getDay()];
  const mm = String(d.getMonth() + 1).padStart(2, "0");
  const dd = String(d.getDate()).padStart(2, "0");
  return `${d.getFullYear()}. ${mm}. ${dd}. (${weekday})`;
}

// Full date + time, unambiguous regardless of when it's viewed — unlike
// formatDateDivider's "오늘"/"어제" shorthand, meant for a fixed record (e.g.
// a recording's saved-at timestamp) rather than a live conversation.
export function formatAbsoluteDateTime(unixSeconds: number): string {
  return `${formatAbsoluteDate(new Date(unixSeconds * 1000))} ${formatMessageTime(unixSeconds)}`;
}
