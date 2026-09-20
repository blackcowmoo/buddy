import { quizChoiceClass } from "../lib/quizCheck";

// Keep selection separate from submission across learning pages, so a
// learner can revise a choice before committing their answer.
export function QuizChoices({ options, selectedIndex, correctIndex, disabled = false, label, lang, onSelect }: {
  options: string[];
  selectedIndex: number | null;
  correctIndex?: number;
  disabled?: boolean;
  label: string;
  lang?: string;
  onSelect?: (index: number) => void;
}) {
  const checked = correctIndex !== undefined;
  return (
    <div className="quiz-choices" role="group" aria-label={label}>
      {options.map((option, index) => {
        const selected = selectedIndex === index;
        const className = quizChoiceClass(checked, selected, correctIndex === index);
        return (
          <button
            key={index}
            type="button"
            className={className + (!checked && selected ? " selected" : "")}
            aria-pressed={selected}
            lang={lang}
            disabled={disabled || checked}
            onClick={() => onSelect?.(index)}
          >
            {option}
          </button>
        );
      })}
    </div>
  );
}
