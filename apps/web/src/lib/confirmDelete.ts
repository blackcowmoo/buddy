import type { Dispatch, SetStateAction } from "react";

// Confirms with the user, deletes via deleteFn, and on success removes the
// matching item from state — the "confirm → delete → filter out" shape
// shared by the chat-room list and the recordings list.
export async function confirmThenDelete<T extends { id: string }>(
  message: string,
  deleteFn: (id: string) => Promise<boolean>,
  id: string,
  setList: Dispatch<SetStateAction<T[]>>,
): Promise<void> {
  if (!window.confirm(message)) return;
  if (await deleteFn(id)) {
    setList((list) => list.filter((item) => item.id !== id));
  }
}
