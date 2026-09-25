import type { NuanceLesson } from "./nuance";

export function nuanceFixture(): NuanceLesson {
 return {
  "id": "lesson-1",
  "status": "done",
  "createdAt": 1700000000,
  "revision": 1,
  "content": {
    "meaning": "저렴한",
    "distinction": "가격과 품질을 구분해요",
    "caveat": "cheap도 중립적으로 쓸 수 있어요.",
    "words": [
      {
        "word": "cheap",
        "tone": "가격 또는 품질",
        "description": "싸구려라는 느낌도 있어요.",
        "example": "This item is cheap.",
        "translation": "이 물건은 저렴해요."
      },
      {
        "word": "inexpensive",
        "tone": "중립적",
        "description": "낮은 가격을 설명해요.",
        "example": "This item is inexpensive.",
        "translation": "이 물건은 저렴해요."
      }
    ],
    "questions": [
      {
        "id": "q0",
        "context": "상황 0: 전달할 느낌을 골라요.",
        "sentence": "Item 0 is ____.",
        "translation": "물건 0의 가격과 품질에 대한 번역",
        "answer": "cheap",
        "explanation": "cheap은 품질을 낮춰 말할 수 있고 inexpensive는 가격을 설명해요."
      },
      {
        "id": "q1",
        "context": "상황 1: 전달할 느낌을 골라요.",
        "sentence": "Item 1 is ____.",
        "translation": "물건 1의 가격과 품질에 대한 번역",
        "answer": "inexpensive",
        "explanation": "cheap은 품질을 낮춰 말할 수 있고 inexpensive는 가격을 설명해요."
      },
      {
        "id": "q2",
        "context": "상황 2: 전달할 느낌을 골라요.",
        "sentence": "Item 2 is ____.",
        "translation": "물건 2의 가격과 품질에 대한 번역",
        "answer": "cheap",
        "explanation": "cheap은 품질을 낮춰 말할 수 있고 inexpensive는 가격을 설명해요."
      },
      {
        "id": "q3",
        "context": "상황 3: 전달할 느낌을 골라요.",
        "sentence": "Item 3 is ____.",
        "translation": "물건 3의 가격과 품질에 대한 번역",
        "answer": "inexpensive",
        "explanation": "cheap은 품질을 낮춰 말할 수 있고 inexpensive는 가격을 설명해요."
      },
      {
        "id": "q4",
        "context": "상황 4: 전달할 느낌을 골라요.",
        "sentence": "Item 4 is ____.",
        "translation": "물건 4의 가격과 품질에 대한 번역",
        "answer": "cheap",
        "explanation": "cheap은 품질을 낮춰 말할 수 있고 inexpensive는 가격을 설명해요."
      }
    ]
  },
  "state": {
    "progress": {},
    "queue": []
  }
};
}
