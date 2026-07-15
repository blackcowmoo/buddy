/**
 * @vitest-environment jsdom
 */
import "@testing-library/jest-dom/vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

vi.mock("../lib/recordings", async () => {
  const actual = await vi.importActual<typeof import("../lib/recordings")>("../lib/recordings");
  return { ...actual, fetchRecordings: vi.fn(), deleteRecording: vi.fn() };
});

import { Recordings } from "./Recordings";
import { deleteRecording, fetchRecordings } from "../lib/recordings";

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("Recordings page", () => {
  it("links back to the chat page with a relative href", () => {
    vi.mocked(fetchRecordings).mockReturnValue(new Promise(() => {}));
    render(<Recordings />);
    expect(screen.getByRole("link", { name: "대화로 돌아가기" })).toHaveAttribute("href", ".");
  });

  it("shows a loading hint before the fetch resolves", () => {
    vi.mocked(fetchRecordings).mockReturnValue(new Promise(() => {})); // never resolves
    render(<Recordings />);
    expect(screen.getByText("불러오는 중…")).toBeInTheDocument();
  });

  it("shows an empty-state hint when there are no recordings", async () => {
    vi.mocked(fetchRecordings).mockResolvedValue([]);
    render(<Recordings />);
    expect(await screen.findByText("아직 저장된 녹음이 없습니다.")).toBeInTheDocument();
  });

  it("shows an error hint when the fetch fails", async () => {
    vi.mocked(fetchRecordings).mockResolvedValue(null);
    render(<Recordings />);
    expect(await screen.findByText(/녹음 목록을 불러오지 못했습니다/)).toBeInTheDocument();
  });

  it("formats duration and size next to each recording", async () => {
    vi.mocked(fetchRecordings).mockResolvedValue([
      { id: "rec-1", createdAt: 1700000000, durationMs: 90000, sizeBytes: 123456 },
    ]);
    render(<Recordings />);

    expect(await screen.findByText(/1:30/)).toBeInTheDocument();
    expect(screen.getByText(/120\.6 KB/)).toBeInTheDocument();
  });

  it("links the audio source to the recording's audio endpoint", async () => {
    vi.mocked(fetchRecordings).mockResolvedValue([
      { id: "rec-1", createdAt: 1700000000, durationMs: 1000, sizeBytes: 100 },
    ]);
    const { container } = render(<Recordings />);

    const audio = await screen.findByText(/0:01/).then(() => container.querySelector("audio"));
    expect(audio).toHaveAttribute("src", "api/recordings/rec-1/audio");
  });

  it("asks for confirmation, deletes, and removes the row on confirmed success", async () => {
    vi.mocked(fetchRecordings).mockResolvedValue([
      { id: "rec-1", createdAt: 1700000000, durationMs: 1000, sizeBytes: 100 },
    ]);
    vi.mocked(deleteRecording).mockResolvedValue(true);
    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(true);
    const user = userEvent.setup();
    render(<Recordings />);

    await user.click(await screen.findByRole("button", { name: "녹음 삭제" }));

    expect(confirmSpy).toHaveBeenCalled();
    expect(deleteRecording).toHaveBeenCalledWith("rec-1");
    expect(await screen.findByText("아직 저장된 녹음이 없습니다.")).toBeInTheDocument();
  });

  it("does not delete when the confirmation is declined", async () => {
    vi.mocked(fetchRecordings).mockResolvedValue([
      { id: "rec-1", createdAt: 1700000000, durationMs: 1000, sizeBytes: 100 },
    ]);
    vi.spyOn(window, "confirm").mockReturnValue(false);
    const user = userEvent.setup();
    render(<Recordings />);

    await user.click(await screen.findByRole("button", { name: "녹음 삭제" }));

    expect(deleteRecording).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "녹음 삭제" })).toBeInTheDocument();
  });

  it("keeps the row when the delete request fails", async () => {
    vi.mocked(fetchRecordings).mockResolvedValue([
      { id: "rec-1", createdAt: 1700000000, durationMs: 1000, sizeBytes: 100 },
    ]);
    vi.mocked(deleteRecording).mockResolvedValue(false);
    vi.spyOn(window, "confirm").mockReturnValue(true);
    const user = userEvent.setup();
    render(<Recordings />);

    await user.click(await screen.findByRole("button", { name: "녹음 삭제" }));

    expect(deleteRecording).toHaveBeenCalledWith("rec-1");
    expect(screen.getByRole("button", { name: "녹음 삭제" })).toBeInTheDocument();
  });
});
