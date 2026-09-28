import type { ComponentProps } from "react";

type Props = Omit<ComponentProps<"input">, "value" | "type" | "className" | "style"> & {
  value: string;
  checked: boolean;
  correct: boolean;
};

export function QuizBlankInput({ value, checked, correct, disabled, ...props }: Props) {
  return (
    <input
      {...props}
      type="text"
      value={value}
      className={`quiz-blank-input${checked ? correct ? " correct" : " incorrect" : ""}`}
      // Size from the learner's input so the hidden answer's length stays hidden.
      style={{ width: `${Math.min(16, Math.max(3, value.length + 1))}ch` }}
      disabled={checked || disabled}
    />
  );
}
