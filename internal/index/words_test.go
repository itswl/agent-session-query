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
		// A word alone: by how a word begins, as the stem itself, and as the stem the
		// table holds beside the words that have another
		"retry":             `("retry"* OR "retri" OR "ǂretri")`,
		"webhook":           `("webhook"* OR "ǂwebhook")`, // its own stem: no second form of it
		"RETRIES":           `("retries"* OR "retri" OR "ǂretri")`,
		"session_query":     `"session query"*`, // punctuation splits, as it does in the index; two words match as written
		`"exit status"`:     `"exit status"`,    // a phrase matches as written
		`"retries"`:         `"retries"`,        // … and so does a word quoted
		"数据库":               `"数 据 库"`,          // each character its own word; no prefix on a single one
		"加索引 retry":         `"加 索 引" AND ("retry"* OR "retri" OR "ǂretri")`,
		"ask:deploy -flaky": `({ask}: ("deploy"* OR "deploi" OR "ǂdeploi")) NOT "flaky"*`, // an excluded word as typed
		"title:webhook":     `{title}: ("webhook"* OR "ǂwebhook")`,
		"-only -negatives":  ``, // nothing to look for
		`say "hi"`:          `("say"* OR "sai" OR "ǂsai") AND "hi"`,
		"go1 v2 ab":         `"go1"* AND "v2"* AND "ab"*`, // digits, and words too short to stem, as they were
		"café":              `"café"*`,
		"c++":               `"c"`,           // what is typed after a word says it ends there
		"f# -c++":           `("f") NOT "c"`, // … excluded as well
		"node.js":           `"node js"*`,    // … and what ends in a word still begins others
		"retry.":            `("retry" OR "retri" OR "ǂretri")`,
		"!!!":               ``,
	} {
		if got := ParseQuery(in).match(); got != want {
			t.Errorf("match(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestStemsBesideTheWords(t *testing.T) {
	for in, want := range map[string]string{
		"Retried the retries; retry the webhook": "ǂretri", // once, for every form; "the" and "webhook" are their own stems
		"logs logging LOGGED login":              "ǂlog",
		"go1.26 utf8 数据库 café ab":                "", // digits, other scripts, accents and short words are left alone
		"":                                       "",
	} {
		if got := stemsOf(in); got != want {
			t.Errorf("stemsOf(%q) = %q, want %q", in, got, want)
		}
	}
	if got := withStems("it retried"); got != "it retried\nǂretri" {
		t.Errorf("withStems = %q: the stems follow the words, on a line of their own", got)
	}

	// A snippet is cut around the typed form, or failing that around another form of it
	for _, c := range []struct {
		needle   needle
		text     string
		at, size int
		word     string
		why      string
	}{
		{needle{text: "retry", stem: "retri"}, "we Retry here", 3, 5, "", "the typed form, any case"},
		{needle{text: "retry", stem: "retri"}, "it was Retried twice", 7, 7, "Retried", "another form, as written"},
		{needle{text: "retry", stem: "retri"}, "retriever retrieval", -1, 0, "", "words that only begin alike"},
		{needle{text: "数据库"}, "给数据库加索引", 3, 9, "", "a CJK run, by substring"},
		{needle{text: "retry", toks: []string{"retry"}, prefix: true}, "ẞ RETRY", 4, 5, "", "where lowering changes a character's length"},
		// A term of several words is its words one after another, whatever is between them
		{needle{text: "session_query", toks: []string{"session", "query"}, prefix: true}, "in Session-Query's tests", 3, 13, "", "the words, any punctuation between"},
		{needle{text: "exit status", toks: []string{"exit", "status"}}, "exit\n  status 1", 0, 13, "", "a phrase over a line's end"},
		{needle{text: "go test", toks: []string{"go", "test"}}, "sed -n 1,80p multipart.go", -1, 0, "", "not a word that only ends a word"},
		{needle{text: "go test", toks: []string{"go", "test"}}, "cargo test; go: testing", -1, 0, "", "nor the words apart, or the last only begun"},
		{needle{text: "go tes", toks: []string{"go", "tes"}, prefix: true}, "cargo test; go: testing", 12, 11, "", "… unless it is a prefix"},
	} {
		at, size, word := c.needle.find(c.text)
		if at != c.at || size != c.size || word != c.word {
			t.Errorf("%s: find(%q) = %d, %d, %q; want %d, %d, %q", c.why, c.text, at, size, word, c.at, c.size, c.word)
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
