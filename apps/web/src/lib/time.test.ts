import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  formatAbsoluteDate,
  formatAbsoluteDateTime,
  formatDateDivider,
  formatMessageTime,
  formatRelativeTime,
  shouldShowDateDivider,
} from "./time";

// "Now" for the clock-dependent helpers: Wed Jul 15 2026, noon local time.
// Every fixture below is built with local Date constructors, so the expected
// strings hold in whatever timezone (and DST regime) the suite runs in; only
// the toLocaleDateString fallback compares against the runtime's own output.
const NOW = new Date(2026, 6, 15, 12, 0, 0);
const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

const unix = (d: Date) => Math.floor(d.getTime() / 1000);
const ago = (ms: number) => unix(new Date(NOW.getTime() - ms));

const freezeClock = () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(NOW);
};

describe("formatRelativeTime", () => {
  beforeEach(freezeClock);
  afterEach(() => vi.useRealTimers());

  it("says 방금 전 for anything under a minute old", () => {
    expect(formatRelativeTime(ago(30 * 1000))).toBe("방금 전");
    expect(formatRelativeTime(unix(NOW))).toBe("방금 전");
  });

  it("says 방금 전 for timestamps slightly in the future", () => {
    // A client clock a few seconds ahead of the server's must not render
    // a negative "-1분 전".
    expect(formatRelativeTime(ago(-5 * 1000))).toBe("방금 전");
  });

  it("counts minutes up to the hour boundary, flooring partial minutes", () => {
    expect(formatRelativeTime(ago(5 * MINUTE))).toBe("5분 전");
    expect(formatRelativeTime(ago(59 * MINUTE + 59_000))).toBe("59분 전");
  });

  it("switches to hours at 60 minutes and floors through the day", () => {
    expect(formatRelativeTime(ago(HOUR))).toBe("1시간 전");
    expect(formatRelativeTime(ago(23 * HOUR + 59 * MINUTE))).toBe("23시간 전");
  });

  it("switches to days at 24 hours and stops at 30 days", () => {
    expect(formatRelativeTime(ago(DAY))).toBe("1일 전");
    expect(formatRelativeTime(ago(29 * DAY + 23 * HOUR))).toBe("29일 전");
  });

  it("falls back to a plain local date once the relative steps run out", () => {
    const ts = ago(30 * DAY);
    expect(formatRelativeTime(ts)).toBe(new Date(ts * 1000).toLocaleDateString());
  });
});

describe("shouldShowDateDivider", () => {
  it("hides the divider when there is no current timestamp", () => {
    expect(shouldShowDateDivider(unix(NOW), null)).toBe(false);
    expect(shouldShowDateDivider(unix(NOW), undefined)).toBe(false);
    expect(shouldShowDateDivider(null, undefined)).toBe(false);
  });

  it("shows the divider at the start of a list, with no earlier item", () => {
    expect(shouldShowDateDivider(null, unix(NOW))).toBe(true);
    expect(shouldShowDateDivider(undefined, unix(NOW))).toBe(true);
  });

  it("keeps timestamps from the same local calendar day together", () => {
    const morning = unix(new Date(2026, 6, 15, 8, 0));
    const evening = unix(new Date(2026, 6, 15, 23, 59));
    expect(shouldShowDateDivider(morning, evening)).toBe(false);
    expect(shouldShowDateDivider(evening, evening)).toBe(false);
  });

  it("splits on the calendar day, not a 24-hour window", () => {
    // The case this helper exists for: two minutes apart, but on either
    // side of local midnight.
    const beforeMidnight = unix(new Date(2026, 6, 14, 23, 59));
    const afterMidnight = unix(new Date(2026, 6, 15, 0, 1));
    expect(shouldShowDateDivider(beforeMidnight, afterMidnight)).toBe(true);
  });
});

describe("formatDateDivider", () => {
  beforeEach(freezeClock);
  afterEach(() => vi.useRealTimers());

  it("labels the current day 오늘, even just after local midnight", () => {
    expect(formatDateDivider(unix(new Date(2026, 6, 15, 0, 1)))).toBe("오늘");
  });

  it("labels the previous day 어제 by its start of day, not its clock time", () => {
    expect(formatDateDivider(unix(new Date(2026, 6, 14, 23, 59)))).toBe("어제");
  });

  it("drops the year inside the current year", () => {
    expect(formatDateDivider(unix(new Date(2026, 6, 10)))).toBe("7월 10일 (금)");
  });

  it("keeps the full date once the year rolls over", () => {
    expect(formatDateDivider(unix(new Date(2025, 11, 31)))).toBe("2025. 12. 31. (수)");
  });
});

describe("formatMessageTime", () => {
  it("renders the Korean 12-hour clock, with midnight and noon as 12", () => {
    const at = (hour: number, minute: number) => formatMessageTime(unix(new Date(2026, 6, 15, hour, minute)));
    expect(at(0, 0)).toBe("오전 12:00");
    expect(at(1, 5)).toBe("오전 1:05");
    expect(at(11, 59)).toBe("오전 11:59");
    expect(at(12, 0)).toBe("오후 12:00");
    expect(at(13, 7)).toBe("오후 1:07");
    expect(at(23, 59)).toBe("오후 11:59");
  });
});

describe("formatAbsoluteDate", () => {
  it("zero-pads month and day and appends the Korean weekday", () => {
    expect(formatAbsoluteDate(new Date(2026, 0, 4))).toBe("2026. 01. 04. (일)");
    expect(formatAbsoluteDate(new Date(2024, 4, 20))).toBe("2024. 05. 20. (월)");
  });
});

describe("formatAbsoluteDateTime", () => {
  it("joins the absolute date and the Korean clock time", () => {
    const ts = unix(new Date(2026, 6, 15, 13, 7));
    expect(formatAbsoluteDateTime(ts)).toBe("2026. 07. 15. (수) 오후 1:07");
  });
});
