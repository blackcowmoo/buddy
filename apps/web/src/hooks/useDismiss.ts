import { useEffect } from "react";

// The distance a pointer may drift and still count as a tap. Waiting until
// pointerup (instead of dismissing on pointerdown) lets a touch that starts
// outside a popover turn into a scroll gesture without making the popover
// disappear underneath the learner's finger.
const TAP_SLOP_PX = 8;

// Closes an open dropdown/popover on an outside tap or Escape. Outside drags
// deliberately stay open so the surrounding scroll surface remains usable
// on narrow touch screens; the panel's ref and its own close callback are the
// only per-instance bits.
export function useDismiss(open: boolean, ref: React.RefObject<HTMLElement | null>, onClose: () => void) {
  useEffect(() => {
    if (!open) return;

    let outsidePress: { pointerId: number; x: number; y: number; moved: boolean } | null = null;

    const onPointerDown = (e: PointerEvent) => {
      if (!ref.current || ref.current.contains(e.target as Node)) return;
      outsidePress = { pointerId: e.pointerId, x: e.clientX, y: e.clientY, moved: false };
    };
    const onPointerMove = (e: PointerEvent) => {
      if (!outsidePress || outsidePress.pointerId !== e.pointerId) return;
      if (Math.hypot(e.clientX - outsidePress.x, e.clientY - outsidePress.y) > TAP_SLOP_PX) {
        outsidePress.moved = true;
      }
    };
    const onPointerUp = (e: PointerEvent) => {
      if (!outsidePress || outsidePress.pointerId !== e.pointerId) return;
      const shouldClose =
        !outsidePress.moved && !!ref.current && !ref.current.contains(e.target as Node);
      outsidePress = null;
      if (shouldClose) onClose();
    };
    const onPointerCancel = () => {
      outsidePress = null;
    };
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    document.addEventListener("pointerdown", onPointerDown);
    document.addEventListener("pointermove", onPointerMove);
    document.addEventListener("pointerup", onPointerUp);
    document.addEventListener("pointercancel", onPointerCancel);
    document.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown);
      document.removeEventListener("pointermove", onPointerMove);
      document.removeEventListener("pointerup", onPointerUp);
      document.removeEventListener("pointercancel", onPointerCancel);
      document.removeEventListener("keydown", onKeyDown);
    };
  }, [open, ref, onClose]);
}
