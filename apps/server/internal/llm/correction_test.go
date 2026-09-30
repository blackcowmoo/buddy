package llm

import "testing"

func TestCorrectOutput(t *testing.T) {
	if got := CorrectOutput("단어 뉤앙스가 잘 드러나요. 뉤앙스가 또 나옵니다."); got != "단어 뉘앙스가 잘 드러나요. 뉘앙스가 또 나옵니다." {
		t.Fatalf("CorrectOutput() = %q", got)
	}
}
