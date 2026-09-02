package journey

import "testing"

func TestRender(t *testing.T) {
	attrs := map[string]string{"first_name": "영주", "city": "서울"}
	cases := map[string]string{
		"안녕 {{first_name}}님":        "안녕 영주님",
		"{{first_name}} @ {{city}}": "영주 @ 서울",
		"{{ first_name }} 공백허용":     "영주 공백허용",
		"{{unknown}} 은 빈값":          " 은 빈값",
		"변수 없음":                     "변수 없음",
	}
	for tmpl, want := range cases {
		if got := Render(tmpl, attrs); got != want {
			t.Errorf("Render(%q) = %q, want %q", tmpl, got, want)
		}
	}
}

func TestParseDefinition(t *testing.T) {
	raw := []byte(`{
		"entry": {"type":"blast","segment_id":"s1"},
		"nodes": [
			{"type":"message","push":{"title":"t","body":"b"}},
			{"type":"delay","duration_seconds":86400},
			{"type":"message","push":{"title":"t2","body":"b2"}}
		],
		"exit": {"conversion_event":"purchase"},
		"settings": {"category":"marketing"}
	}`)
	def, err := ParseDefinition(raw)
	if err != nil {
		t.Fatalf("ParseDefinition: %v", err)
	}
	if def.Entry.Type != "blast" || len(def.Nodes) != 3 {
		t.Errorf("파싱 결과 불일치: %+v", def)
	}
	if def.Nodes[1].Type != "delay" || def.Nodes[1].DurationSeconds != 86400 {
		t.Errorf("delay 노드 파싱 실패: %+v", def.Nodes[1])
	}
	if def.Nodes[0].Push == nil || def.Nodes[0].Push.Title != "t" {
		t.Errorf("message 노드 파싱 실패: %+v", def.Nodes[0])
	}
	if def.Exit.ConversionEvent != "purchase" {
		t.Errorf("exit 파싱 실패: %+v", def.Exit)
	}
}

func TestMergeAttrs(t *testing.T) {
	std := []byte(`{"first_name":"영주","country":"KR"}`)
	custom := []byte(`{"vip_level":3,"tags":["a","b"]}`)
	attrs := mergeAttrs(std, custom)
	if attrs["first_name"] != "영주" || attrs["country"] != "KR" {
		t.Errorf("표준 속성 병합 실패: %v", attrs)
	}
	if attrs["vip_level"] != "3" {
		t.Errorf("숫자 속성 문자열화 실패: %q", attrs["vip_level"])
	}
	if attrs["tags"] != `["a","b"]` {
		t.Errorf("배열 속성 직렬화 실패: %q", attrs["tags"])
	}
}
