package index

import (
	"database/sql"
	"strings"
	"testing"
)

// porterOf stems words with SQLite's own FTS5 porter tokenizer: each word a row of a table
// that tokenizes with it, and the term the row holds read back from its vocabulary.
func porterOf(t *testing.T, words []string) map[string]string {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, q := range []string{
		`CREATE VIRTUAL TABLE p USING fts5 (w, tokenize = 'porter ascii')`,
		`CREATE VIRTUAL TABLE pv USING fts5vocab (p, 'instance')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i, w := range words {
		if _, err := tx.Exec(`INSERT INTO p (rowid, w) VALUES (?, ?)`, i+1, w); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT doc, term FROM pv`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var doc int
		var term string
		if err := rows.Scan(&doc, &term); err != nil {
			t.Fatal(err)
		}
		out[words[doc-1]] = term
	}
	return out
}

// Words with the endings every rule of every step is written for, and the awkward ones:
// a y after a vowel and after a consonant, doubled consonants, words of three letters.
func stemWords() []string {
	words := strings.Fields(`
		caresses ponies ties caress cats feed agreed plastered bled motoring sing conflated
		troubled sized hopping tanned falling hissing fizzed failing filing happy sky relational
		conditional rational valenci hesitanci digitizer conformabli radicalli differentli vileli
		analogousli vietnamization predication operator feudalism decisiveness hopefulness
		callousness formaliti sensitiviti sensibiliti triplicate formative formalize electriciti
		electrical hopeful goodness revival allowance inference airliner gyroscopic adjustable
		defensible irritant replacement adjustment dependent adoption homologou communism activate
		angulariti homologous effective bowdlerize probate rate cease controll roll generalization
		retry retries retried retrying retryable webhook webhooks deployment deploym logs logging
		login cors core status exited configuration configurat rebind rebinding archaeology analogi
		toy toys played playing say says enjoy yes yet eyes buy buys aed ing eed ies sses ss sssss
		yyy xyz aaa bbb qqqq oed ied eyed dyed abled bling zzzing faxed boxing flowed snowing
		tried flies dying lying dies agreement eement ement ment lement statement`)
	roots := strings.Fields(`connect relate general fail retry test build run hop file plan sign happy
		nation format sense depend adjust conduct predict trouble create hope cause control agree`)
	suffixes := strings.Fields(`s es ed ing er ers ion ions ions ional ionally ation ational ations
		ness ful fully ive ively iveness able ably ability ible ism ist ize izer ization ise ment ments
		ance ence ant ent ly ally al ality ous ously ousness ate ated ating ator ic ical ically iti y ies`)
	for _, r := range roots {
		for _, s := range suffixes {
			words = append(words, r+s)
		}
	}
	return words
}

func TestStemIsFTS5Porter(t *testing.T) {
	words := stemWords()
	want := porterOf(t, words)
	if len(want) != len(stemDistinct(words)) {
		t.Fatalf("read %d stems back for %d words", len(want), len(stemDistinct(words)))
	}
	for _, w := range words {
		if got := stem(w); got != want[w] {
			t.Errorf("stem(%q) = %q, FTS5's porter has %q", w, got, want[w])
		}
	}
}

func stemDistinct(words []string) map[string]bool {
	m := map[string]bool{}
	for _, w := range words {
		m[w] = true
	}
	return m
}

func TestStem(t *testing.T) {
	for in, want := range map[string]string{
		"retry": "retri", "retries": "retri", "retried": "retri", "retrying": "retri",
		"logs": "log", "logging": "log", "logged": "log", "login": "login",
		"webhooks": "webhook",
		// What the stemmer leaves alone: short words, digits, other alphabets, capitals
		// (the words table lowercases before it stems), and words past 64 letters
		"is": "is", "go1": "go1", "utf8": "utf8", "café": "café", "Retries": "Retries", "数据": "数据",
		strings.Repeat("ab", 33) + "ing": strings.Repeat("ab", 33) + "ing",
	} {
		if got := stem(in); got != want {
			t.Errorf("stem(%q) = %q, want %q", in, got, want)
		}
	}
}
