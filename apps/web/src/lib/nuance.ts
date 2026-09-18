import { readStored, writeStored } from "./storedValue";

export interface NuanceWord {
  word: string;
  tone: string;
  description: string;
  example: string;
  translation: string;
}

export interface NuanceQuestion {
  id: string;
  context: string;
  sentence: string;
  translation: string;
  answer: string;
  explanation: string;
}

export interface NuanceLesson {
  id: string;
  meaning: string;
  distinction: string;
  caveat: string;
  words: NuanceWord[];
  questions: NuanceQuestion[];
}

// Explanations and examples are authored for these specific contexts; dictionary
// links in the comparison view let learners check the broader senses of a word.
export const nuanceLessons: NuanceLesson[] = [
  {
    id: "price", meaning: "값이 싼", distinction: "낮은 가격을 말할까, 싼 티가 난다고 말할까?",
    caveat: "cheap도 저렴한 항공권처럼 중립적·긍정적으로 쓸 수 있어요. 항상 품질이 나쁘다는 뜻은 아니에요.",
    words: [
      { word: "cheap", tone: "저렴함 · 문맥에 따라 낮은 품질", description: "가격이 낮다는 뜻이에요. 물건의 외관이나 재료를 평가할 때는 싸구려라는 느낌도 줄 수 있어요.", example: "The handle feels cheap and flimsy.", translation: "손잡이가 싸구려 같고 약하게 느껴져요." },
      { word: "inexpensive", tone: "가격이 낮다는 중립적 설명", description: "돈이 많이 들지 않는다는 점에 초점을 둬요. 품질을 깎아내리는 느낌 없이 가격을 설명하기 좋아요.", example: "We found an inexpensive, well-made desk.", translation: "우리는 저렴하면서도 잘 만든 책상을 찾았어요." },
    ],
    questions: [
      { id: "price-quality", context: "가격표는 보지 않았어요. 손잡이가 금방 부러질 듯해서 ‘싸구려 같다’는 불만을 표현하려고 해요.", sentence: "This handle feels ____.", translation: "이 손잡이는 싸구려 같아요.", answer: "cheap", explanation: "cheap은 낮은 품질에 대한 부정적 평가를 담을 수 있어요. inexpensive는 비용이 낮다는 뜻이라, 가격을 모르는 상황에서 품질에 대한 불만을 전하기에는 맞지 않아요." },
      { id: "price-recommend", context: "잘 만든 책상을 추천하는 안내문이에요. 싸구려라는 오해를 피하고 가격이 낮다는 사실만 전달하고 싶어요.", sentence: "This desk is well-made and ____.", translation: "이 책상은 잘 만들어졌고 가격도 저렴해요.", answer: "inexpensive", explanation: "inexpensive가 품질을 낮춰 말하지 않고 가격을 설명하려는 의도에 더 잘 맞아요. cheap도 이 문장에서 가능하지만, 문맥에 따라 싸구려라는 느낌을 줄 수 있어요." },
    ],
  },
  {
    id: "curiosity", meaning: "궁금해하는", distinction: "알고 싶은 마음일까, 사생활을 캐묻는 걸까?",
    caveat: "curious는 이상하거나 특이하다는 뜻도 있어요. 여기서는 무언가를 알고 싶어 하는 사람을 비교해요.",
    words: [
      { word: "curious", tone: "중립적 · 호기심", description: "새로운 사실이나 원리를 알고 싶어 하는 마음이에요. 배움에 대한 관심을 좋게 표현할 수 있어요.", example: "Mina is curious about how clouds form.", translation: "미나는 구름이 어떻게 생기는지 궁금해해요." },
      { word: "nosy", tone: "부정적 · 지나친 간섭", description: "남의 개인적인 일을 필요 이상으로 알아내려 한다는 느낌이에요. 상대의 경계를 넘는 호기심을 비판해요.", example: "My nosy neighbor keeps asking about my salary.", translation: "참견하기 좋아하는 이웃이 자꾸 내 월급을 물어봐요." },
    ],
    questions: [
      { id: "curiosity-science", context: "아이가 별이 빛나는 이유를 질문해요. 배우려는 태도를 칭찬하고 싶어요.", sentence: "She's ____ about the night sky.", translation: "그 아이는 밤하늘에 호기심이 많아요.", answer: "curious", explanation: "curious는 알고 싶어 하는 마음을 표현해요. nosy를 쓰면 남의 사생활을 캐묻는다는 비판이 되어, 과학에 대한 관심을 칭찬하려는 의도와 달라져요." },
      { id: "curiosity-private", context: "동료가 대답하기 싫다고 했는데도 연애사를 계속 캐물어요. 그 간섭을 못마땅하게 표현해요.", sentence: "Stop being so ____ about her private life.", translation: "그 사람의 사생활을 그렇게 캐묻지 마세요.", answer: "nosy", explanation: "nosy는 과도한 관심과 간섭에 대한 불쾌감을 담아요. curious는 궁금하다는 뜻만으로도 쓰여서, 사생활의 경계를 넘었다는 비판이 덜 분명해요." },
    ],
  },
  {
    id: "confidence", meaning: "자신감이 있는", distinction: "자신을 믿는 걸까, 남보다 우월하다고 여기는 걸까?",
    caveat: "자신감의 크기만으로 나누지 마세요. arrogant에는 남보다 자신이 더 중요하거나 뛰어나다고 여기는 부정적인 평가가 있어요.",
    words: [
      { word: "confident", tone: "자기 능력에 대한 믿음", description: "자신의 능력이나 성공 가능성을 믿는다는 뜻이에요. 다른 사람을 무시한다는 의미는 없어요.", example: "She's confident in her ability to lead the team.", translation: "그녀는 팀을 이끌 자신의 능력을 믿어요." },
      { word: "arrogant", tone: "부정적 · 오만함", description: "자신이 남보다 낫다고 여기며 불쾌하게 구는 태도를 비판해요. 단순히 자신감 있다는 칭찬으로 쓰지 않아요.", example: "His arrogant reply made the team feel ignored.", translation: "그의 오만한 대답에 팀원들은 무시당했다고 느꼈어요." },
    ],
    questions: [
      { id: "confidence-ready", context: "발표자가 충분히 준비해서 자기 능력을 믿고 있어요. 질문과 조언도 존중하는 태도를 칭찬해요.", sentence: "She's a ____ presenter who welcomes feedback.", translation: "그녀는 피드백을 환영하는 자신감 있는 발표자예요.", answer: "confident", explanation: "confident는 준비와 능력에 대한 믿음을 표현해요. arrogant는 남보다 우월하다는 오만함을 비판하므로, 이 칭찬에는 맞지 않아요." },
      { id: "confidence-dismiss", context: "‘너희는 나보다 멍청하니까 들을 필요 없어’라고 말한 사람의 태도를 비판해요.", sentence: "You sounded ____ when you said that.", translation: "그렇게 말하니 오만하게 들렸어요.", answer: "arrogant", explanation: "arrogant에는 남을 낮춰 보는 부정적 태도가 담겨 있어요. confident는 자신을 믿는다는 뜻만으로도 쓰이므로, 상대를 무시했다는 핵심을 전달하지 못해요." },
    ],
  },
  {
    id: "child", meaning: "아이 같은", distinction: "순수함을 칭찬할까, 미성숙함을 비판할까?",
    caveat: "childish는 아이의 글씨처럼 단순히 어린이다운 특징을 말하기도 해요. 여기서는 성인의 태도를 평가하는 상황이에요.",
    words: [
      { word: "childlike", tone: "긍정적 · 순수함", description: "아이의 좋은 특성인 순수함, 솔직함, 경이로움 등을 성인에게서 볼 때 써요.", example: "He watched the snow with childlike wonder.", translation: "그는 아이 같은 경이로움으로 눈을 바라봤어요." },
      { word: "childish", tone: "부정적 · 유치함", description: "성인의 행동을 말할 때는 나이에 맞지 않게 철없고 미숙하다고 비판하는 느낌이에요.", example: "Refusing to speak over a small disagreement was childish.", translation: "작은 의견 차이 때문에 말을 안 하겠다는 건 유치했어요." },
    ],
    questions: [
      { id: "child-wonder", context: "할머니가 처음 본 오로라에 순수하게 감탄해요. 그 모습을 따뜻하게 묘사하고 싶어요.", sentence: "She looked at the sky with ____ wonder.", translation: "그녀는 아이 같은 경이로움으로 하늘을 바라봤어요.", answer: "childlike", explanation: "childlike는 아이가 가진 좋은 특성인 순수한 감탄을 표현해요. childish를 쓰면 유치하고 미숙하다는 평가로 들릴 수 있어요." },
      { id: "child-sulk", context: "성인 친구가 게임에서 졌다는 이유로 다른 사람의 물건을 숨겼어요. 그 유치한 행동을 지적해요.", sentence: "Hiding her bag because you lost was ____.", translation: "졌다고 그 사람 가방을 숨긴 건 유치했어요.", answer: "childish", explanation: "childish는 성인의 미숙하고 철없는 행동을 비판하기에 맞아요. childlike는 보통 아이의 좋은 특성을 가리켜서 이 비판의 의도와 달라져요." },
    ],
  },
  {
    id: "reputation", meaning: "유명한", distinction: "널리 알려진 걸까, 나쁜 일로 악명 높은 걸까?",
    caveat: "famous 자체가 반드시 칭찬은 아니에요. 널리 알려졌다는 뜻이고, notorious는 나쁜 평판까지 더 분명히 담아요.",
    words: [
      { word: "famous", tone: "널리 알려짐", description: "많은 사람이 알고 있다는 뜻이에요. 성과나 인기 때문에 알려진 사람과 장소에도 쓸 수 있어요.", example: "The town is famous for its gardens.", translation: "그 마을은 정원으로 유명해요." },
      { word: "notorious", tone: "부정적 · 악명", description: "나쁜 행동이나 문제 때문에 널리 알려졌다는 평가를 담아요.", example: "The company is notorious for ignoring complaints.", translation: "그 회사는 불만을 무시하기로 악명 높아요." },
    ],
    questions: [
      { id: "reputation-art", context: "아름다운 작품으로 세계적인 사랑을 받는 화가를 소개해요. 나쁜 평판을 암시하고 싶지 않아요.", sentence: "She's a ____ painter admired around the world.", translation: "그녀는 세계적으로 사랑받는 유명한 화가예요.", answer: "famous", explanation: "famous는 널리 알려진 화가를 소개하기에 맞아요. notorious는 나쁜 이유로 알려졌다는 인상을 주므로, 이 소개의 의도에 맞지 않아요." },
      { id: "reputation-scams", context: "고객을 속여 돈을 빼앗는 것으로 널리 알려진 업체예요. 악명 높다는 점을 명확히 경고해요.", sentence: "The agency is ____ for scamming customers.", translation: "그 업체는 고객을 속이는 것으로 악명 높아요.", answer: "notorious", explanation: "notorious가 사기로 생긴 나쁜 평판을 분명히 전달해요. famous도 알려졌다는 뜻은 되지만, 악명을 강조하려는 의도는 notorious가 더 정확하게 담아요." },
    ],
  },
  {
    id: "persistence", meaning: "쉽게 포기하지 않는", distinction: "끈기를 말할까, 바꾸지 않으려는 고집을 말할까?",
    caveat: "persistent도 집요한 전화나 계속되는 문제처럼 부정적으로 쓸 수 있어요. 여기서는 노력의 지속과 생각을 바꾸지 않는 태도를 비교해요.",
    words: [
      { word: "persistent", tone: "계속 시도하는 끈기", description: "어려움이 있어도 행동이나 노력을 계속한다는 뜻이에요. 목표를 향한 꾸준한 노력을 칭찬할 때 쓸 수 있어요.", example: "Her persistent efforts finally paid off.", translation: "그녀의 끈질긴 노력이 마침내 결실을 맺었어요." },
      { word: "stubborn", tone: "생각을 바꾸지 않는 고집", description: "다른 사람의 설득에도 생각이나 행동을 바꾸려 하지 않는 태도예요. 융통성 없음을 비판할 수 있어요.", example: "He was too stubborn to admit the plan had failed.", translation: "그는 너무 고집이 세서 계획이 실패했다는 걸 인정하지 않았어요." },
    ],
    questions: [
      { id: "persistence-effort", context: "실패할 때마다 조언을 듣고 방법을 바꾸면서 계속 연구한 팀의 끈기를 칭찬해요.", sentence: "Their ____ efforts led to a breakthrough.", translation: "그들의 끈질긴 노력이 돌파구로 이어졌어요.", answer: "persistent", explanation: "persistent는 노력을 계속했다는 점을 강조해요. stubborn을 쓰면 조언을 거부하거나 태도를 바꾸지 않는 고집이 떠오를 수 있어요." },
      { id: "persistence-refusal", context: "계산이 틀렸다는 명확한 증거를 보여 줬는데도, 인정하기 싫어서 답을 바꾸지 않아요. 그 고집을 비판해요.", sentence: "He's too ____ to admit his mistake.", translation: "그는 너무 고집이 세서 자기 실수를 인정하지 않아요.", answer: "stubborn", explanation: "stubborn은 설득이나 증거에도 태도를 바꾸려 하지 않는 고집을 나타내요. persistent는 행동을 계속한다는 데 초점이 있어, 인정하지 않는 태도를 지적하려는 이 문맥에는 stubborn이 더 잘 맞아요." },
    ],
  },
];

export type NuanceAnswers = Record<string, string>;
const progressKey = "buddy.nuance.answers.v1";

export function loadNuanceAnswers(): NuanceAnswers {
  return readStored(progressKey, (raw) => {
    const parsed: unknown = JSON.parse(raw);
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) return undefined;
    const answers: NuanceAnswers = {};
    for (const lesson of nuanceLessons) {
      for (const question of lesson.questions) {
        const value = (parsed as Record<string, unknown>)[question.id];
        if (lesson.words.some(({ word }) => word === value)) answers[question.id] = value as string;
      }
    }
    return answers;
  }, {});
}

export function saveNuanceAnswers(answers: NuanceAnswers): boolean {
  const raw = JSON.stringify(answers);
  writeStored(progressKey, raw);
  return readStored(progressKey, (stored) => stored === raw, false);
}
