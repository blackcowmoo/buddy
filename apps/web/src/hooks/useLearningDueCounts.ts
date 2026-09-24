import { useEffect, useState } from "react";
import { dueQuestions, fetchNuanceLessons } from "../lib/nuance";
import { fetchWords } from "../lib/wordReview";

export interface LearningDueCounts {
  wordDueCount: number;
  nuanceDueCount: number;
}

// Both the main menu and every subpage menu use this hook, so review badges
// stay consistent no matter where the learner opens the navigation.
export function useLearningDueCounts(): LearningDueCounts {
  const [wordDueCount, setWordDueCount] = useState(0);
  const [nuanceDueCount, setNuanceDueCount] = useState(0);

  useEffect(() => {
    let active = true;
    void fetchWords().then((result) => {
      if (active && result) setWordDueCount(result.dueCount);
    });
    void fetchNuanceLessons().then((lessons) => {
      if (active && lessons) setNuanceDueCount(lessons.reduce((sum, lesson) => sum + dueQuestions(lesson), 0));
    });
    return () => { active = false; };
  }, []);

  return { wordDueCount, nuanceDueCount };
}
