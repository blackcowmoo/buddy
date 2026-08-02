import type { Theme } from "../lib/theme";

const THEME_LABELS: Record<Theme, string> = {
  white: "화이트",
  dark: "다크",
  system: "시스템 설정",
};

export function ThemeSwitch({ theme, onChange }: { theme: Theme; onChange: (t: Theme) => void }) {
  return (
    <div className="theme-switch" role="group" aria-label="테마">
      {(Object.keys(THEME_LABELS) as Theme[]).map((t) => (
        <button
          key={t}
          type="button"
          className={`theme-option ${theme === t ? "active" : ""}`}
          aria-pressed={theme === t}
          onClick={() => onChange(t)}
        >
          {THEME_LABELS[t]}
        </button>
      ))}
    </div>
  );
}
