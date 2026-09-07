import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import { fetchMe } from "../lib/me";
import { fetchSettings, MAX_INTERLOCUTOR_STYLE_LEN, saveSettings } from "../lib/settings";
import { applyTheme, getStoredTheme, onSystemThemeChange, setStoredTheme, type Theme } from "../lib/theme";
import { fetchWords } from "../lib/wordReview";
import {
  NATIVE_RATE,
  loadAutoReadAloud,
  loadPlaybackRate,
  saveAutoReadAloud,
  savePlaybackRate,
} from "../lib/ttsSettings";

// User-wide preferences are independent from the currently open room. Keeping
// their loading, persistence, and UI status together prevents App's chat state
// machine from also becoming a settings state machine.
export function useUserPreferences() {
  const [email, setEmail] = useState<string | null>(null);
  const [theme, setTheme] = useState<Theme>(() => getStoredTheme());
  const [autoReadAloud, setAutoReadAloud] = useState(() => loadAutoReadAloud());
  const autoReadAloudRef = useRef(false);
  const [playbackRate, setPlaybackRate] = useState(() => loadPlaybackRate());
  const playbackRateRef = useRef(NATIVE_RATE);
  const [styleInput, setStyleInput] = useState("");
  const [styleSaving, setStyleSaving] = useState(false);
  const [styleSaved, setStyleSaved] = useState(false);
  const [styleLoadError, setStyleLoadError] = useState(false);
  const [styleSaveError, setStyleSaveError] = useState<string | null>(null);
  const [learnerProfile, setLearnerProfile] = useState("");
  const [wordDueCount, setWordDueCount] = useState(0);

  useEffect(() => {
    autoReadAloudRef.current = autoReadAloud;
    saveAutoReadAloud(autoReadAloud);
  }, [autoReadAloud]);

  useEffect(() => {
    playbackRateRef.current = playbackRate;
    savePlaybackRate(playbackRate);
  }, [playbackRate]);

  useEffect(() => {
    void fetchMe().then((identity) => {
      setEmail(identity?.identityMode === "oidc" ? identity.id : null);
    });
    void fetchSettings().then((settings) => {
      if (!settings) {
        setStyleLoadError(true);
        return;
      }
      setStyleInput(settings.interlocutorStyle);
      setLearnerProfile(settings.learnerProfile);
    });
    void fetchWords().then((result) => {
      if (result) setWordDueCount(result.dueCount);
    });
  }, []);

  useEffect(() => {
    applyTheme(theme);
    if (theme !== "system") return;
    return onSystemThemeChange(() => applyTheme("system"));
  }, [theme]);

  const selectTheme = useCallback((next: Theme) => {
    setStoredTheme(next);
    setTheme(next);
  }, []);

  const submitStyle = useCallback(
    async (event: FormEvent) => {
      event.preventDefault();
      if (styleLoadError) return;
      setStyleSaved(false);
      setStyleSaveError(null);
      const trimmed = styleInput.trim();
      const length = Array.from(trimmed).length;
      if (length > MAX_INTERLOCUTOR_STYLE_LEN) {
        setStyleSaveError(`${MAX_INTERLOCUTOR_STYLE_LEN}자를 초과했습니다 (현재 ${length}자).`);
        return;
      }
      setStyleSaving(true);
      const saved = await saveSettings(trimmed);
      setStyleSaving(false);
      if (!saved) {
        setStyleSaveError("저장하지 못했습니다. 다시 시도해주세요.");
        return;
      }
      setStyleInput(trimmed);
      setStyleSaved(true);
    },
    [styleInput, styleLoadError],
  );

  const handleStyleInputChange = useCallback((value: string) => {
    setStyleInput(value);
    setStyleSaved(false);
    setStyleSaveError(null);
  }, []);

  return {
    email,
    theme,
    selectTheme,
    autoReadAloud,
    setAutoReadAloud,
    autoReadAloudRef,
    playbackRate,
    setPlaybackRate,
    playbackRateRef,
    styleInput,
    styleSaving,
    styleSaved,
    styleLoadError,
    styleSaveError,
    learnerProfile,
    wordDueCount,
    submitStyle,
    handleStyleInputChange,
  };
}
