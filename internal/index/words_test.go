package index

import (
	"reflect"
	"testing"
)

func TestParseQuery(t *testing.T) {
	for in, want := range map[string][]Term{
		"retry backoff":         {{Text: "retry"}, {Text: "backoff"}},
		`"exit status 1" make`:  {{Text: "exit status 1", Exact: true}, {Text: "make"}},
		"-flaky test":           {{Text: "flaky", Not: true}, {Text: "test"}},
		"ask:deploy":            {{Text: "deploy", Column: "ask"}},
		`OUTPUT:"no such file"`: {{Text: "no such file", Column: "output", Exact: true}},
		"-tool:rm":              {{Text: "rm", Column: "tool", Not: true}},
		"http://host:8080":      {{Text: "http://host:8080"}}, // a colon is a prefix only after a part's name
		"ask: deploy":           {{Text: "ask:"}, {Text: "deploy"}},
		`"unclosed phrase`:      {{Text: "unclosed phrase", Exact: true}},
		"  ":                    nil,
		"- alone":               {{Text: "alone"}}, // a dash on its own asks for nothing
	} {
		if got := ParseQuery(in).Terms; !reflect.DeepEqual(got, want) {
			t.Errorf("ParseQuery(%q)\n got %+v\nwant %+v", in, got, want)
		}
	}
}

func TestMatchExpression(t *testing.T) {
	for in, want := range map[string]string{
		"retry":             `"retry"*`,
		"session_query":     `"session query"*`, // punctuation splits, as it does in the index
		`"exit status"`:     `"exit status"`,    // a phrase matches as written
		"数据库":               `"数 据 库"`,          // each character its own word; no prefix on a single one
		"加索引 retry":         `"加 索 引" AND "retry"*`,
		"ask:deploy -flaky": `({ask}: "deploy"*) NOT "flaky"*`,
		"-only -negatives":  ``, // nothing to look for
		`say "hi"`:          `"say"* AND "hi"`,
		"!!!":               ``,
	} {
		if got := ParseQuery(in).match(); got != want {
			t.Errorf("match(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestSegment(t *testing.T) {
	for in, want := range map[string]string{
		"plain ascii":  "plain ascii",
		"café naïve":   "café naïve", // accents are letters, not a spaceless script
		"修复bug":        " 修  复 bug",
		"データ base":     " デ ー タ  base", // the long-vowel mark belongs to no one script: it stands as it is, between two that are set apart
		"한글":           " 한  글 ",
		"ภาษา":         " ภ  า  ษ  า ",
		"":             "",
		"mixed 中 text": "mixed  中  text",
	} {
		if got := segment(in); got != want {
			t.Errorf("segment(%q) = %q, want %q", in, got, want)
		}
	}
	if got := tokens("Fix-the_flaky.TEST 数据 v2"); !reflect.DeepEqual(got, []string{"fix", "the", "flaky", "test", "数", "据", "v2"}) {
		t.Errorf("tokens = %q", got)
	}
}

func TestInputTextIsTheSameEveryTime(t *testing.T) {
	raw := []byte(`{"new_string":"gamma","file_path":"/a/b.go","old_string":"beta","nested":{"z":"last","a":["one","two"]},"count":3}`)
	want := "/a/b.go\none\ntwo\nlast\ngamma\nbeta"
	for range 50 {
		if got := inputText(raw); got != want {
			t.Fatalf("inputText = %q, want %q: the values in the order of their keys, numbers and keys left out", got, want)
		}
	}
	if inputText([]byte(`not json`)) != "" || inputText(nil) != "" {
		t.Error("what is not JSON has no words")
	}
}
