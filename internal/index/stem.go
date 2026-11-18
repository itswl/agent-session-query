package index

// The stems of English words, so that a search for "retries" finds "retried" and "retry".
//
// The stemmer is Porter's, as SQLite's FTS5 has it in its porter tokenizer, step for step:
// the same word gives the same stem here as there (stem_test.go holds the two side by side).
// It is not that tokenizer that the words table uses, because a tokenizer that stems keeps
// only the stems: "deploym" would no longer find "deployment" (the index would hold
// "deploy"), "retry" would no longer find "retryable" ("retri" begins neither), "logs"
// would find "login" (as "log"*), and a quoted phrase would find every other form of its
// words. So the words keep their own forms, as they always did, and the stems of those that
// have another one go beside them, marked (see stemMark).

// stemMark opens a stem in the words table: "retried" is followed there by "ǂretri". It is
// a letter, so the tokenizer keeps it as part of the word, and one nobody types, so no
// search for a word, whole or as a prefix, finds a stem by accident — the stems are found
// only by the expression that asks for one (see Query.match).
const stemMark = "ǂ"

// maxStemmed is the longest word the stemmer takes, as in FTS5: a longer one is no English
// word, and stands as it is.
const maxStemmed = 64

// stemmable reports whether a token is a word the stemmer knows what to do with: three to
// sixty-four lowercase ASCII letters. Anything else — a number, an identifier with digits in
// it, a word in another alphabet — is left as it is.
func stemmable(w string) bool {
	if len(w) < 3 || len(w) > maxStemmed {
		return false
	}
	for i := 0; i < len(w); i++ {
		if w[i] < 'a' || w[i] > 'z' {
			return false
		}
	}
	return true
}

// stem is the Porter stem of a lowercase word: "retries" and "retried" are both "retri". A
// word stemmable does not accept comes back as it was.
func stem(w string) string {
	if !stemmable(w) {
		return w
	}
	var buf [maxStemmed + 1]byte // one more: step 1b can lengthen a word by a letter
	b := buf[:copy(buf[:], w)]

	b = step1a(b)
	if b2, cut := step1b(b); cut {
		b = b2
		if b3, ok := step1b2(b); ok {
			b = b3
		} else if c := b[len(b)-1]; len(b) > 1 && !isVowel(c, false) && c != 'l' && c != 's' && c != 'z' && c == b[len(b)-2] {
			b = b[:len(b)-1]
		} else if mEq1(b) && oStar(b) {
			b = append(b, 'e')
		}
	} else {
		b = b2
	}
	// Step 1c
	if b[len(b)-1] == 'y' && hasVowel(b[:len(b)-1]) {
		b[len(b)-1] = 'i'
	}
	b = step2(b)
	b = step3(b)
	b = step4(b)
	// Step 5a
	if b[len(b)-1] == 'e' {
		if s := b[:len(b)-1]; mGt1(s) || (mEq1(s) && !oStar(s)) {
			b = s
		}
	}
	// Step 5b
	if len(b) > 1 && b[len(b)-1] == 'l' && b[len(b)-2] == 'l' && mGt1(b[:len(b)-1]) {
		b = b[:len(b)-1]
	}
	return string(b)
}

func isVowel(c byte, yIsVowel bool) bool {
	return c == 'a' || c == 'e' || c == 'i' || c == 'o' || c == 'u' || (yIsVowel && c == 'y')
}

// gobbleVC is how far into s the first vowel-then-consonant sequence ends, or 0 when there
// is none. A y is a vowel after a consonant.
func gobbleVC(s []byte, prevCons bool) int {
	cons := prevCons
	i := 0
	for ; i < len(s); i++ {
		if cons = !isVowel(s[i], cons); !cons {
			break
		}
	}
	for i++; i < len(s); i++ {
		if cons = !isVowel(s[i], cons); cons {
			return i + 1
		}
	}
	return 0
}

// The conditions Porter's rules are written in: m is the number of vowel-consonant
// sequences in the stem.
func mGt0(s []byte) bool { return gobbleVC(s, false) > 0 }

func mGt1(s []byte) bool {
	n := gobbleVC(s, false)
	return n > 0 && gobbleVC(s[n:], true) > 0
}

func mEq1(s []byte) bool {
	n := gobbleVC(s, false)
	return n > 0 && gobbleVC(s[n:], true) == 0
}

// oStar: the stem ends consonant-vowel-consonant, the last not w, x or y.
func oStar(s []byte) bool {
	if len(s) == 0 {
		return false
	}
	if c := s[len(s)-1]; c == 'w' || c == 'x' || c == 'y' {
		return false
	}
	mask, cons := 0, false
	for _, c := range s {
		cons = !isVowel(c, cons)
		mask = (mask<<1 | b2i(cons)) & 7
	}
	return mask == 5
}

// hasVowel: the stem holds a vowel, a y counting as one anywhere but at the start (as FTS5
// has it).
func hasVowel(s []byte) bool {
	for i, c := range s {
		if isVowel(c, i > 0) {
			return true
		}
	}
	return false
}

func mGt1ST(s []byte) bool {
	return len(s) > 0 && (s[len(s)-1] == 's' || s[len(s)-1] == 't') && mGt1(s)
}

func hasSuffix(b []byte, suffix string) bool {
	return len(b) > len(suffix) && string(b[len(b)-len(suffix):]) == suffix
}

func step1a(b []byte) []byte {
	n := len(b)
	if b[n-1] != 's' {
		return b
	}
	switch {
	case b[n-2] == 'e':
		if (n > 4 && b[n-4] == 's' && b[n-3] == 's') || (n > 3 && b[n-3] == 'i') {
			return b[:n-2] // sses → ss, ies → i
		}
		return b[:n-1]
	case b[n-2] != 's':
		return b[:n-1]
	}
	return b
}

// step1b removes -eed, -ed and -ing; cut says it removed one of the last two.
func step1b(b []byte) ([]byte, bool) {
	switch {
	case hasSuffix(b, "eed"):
		if mGt0(b[:len(b)-3]) {
			return b[:len(b)-1], false
		}
	case hasSuffix(b, "ed"):
		if hasVowel(b[:len(b)-2]) {
			return b[:len(b)-2], true
		}
	case hasSuffix(b, "ing"):
		if hasVowel(b[:len(b)-3]) {
			return b[:len(b)-3], true
		}
	}
	return b, false
}

func step1b2(b []byte) ([]byte, bool) {
	for _, r := range [...][2]string{{"at", "ate"}, {"bl", "ble"}, {"iz", "ize"}} {
		if hasSuffix(b, r[0]) {
			return append(b[:len(b)-len(r[0])], r[1]...), true
		}
	}
	return b, false
}

// A rule replaces a suffix when the stem before it meets a condition. Of the rules whose
// suffix a word ends with, only the first is tried: when its condition fails, the word
// stands.
type rule struct {
	suffix, with string
	cond         func([]byte) bool
}

func apply(b []byte, rules []rule) []byte {
	for _, r := range rules {
		if hasSuffix(b, r.suffix) {
			if s := b[:len(b)-len(r.suffix)]; r.cond(s) {
				return append(s, r.with...)
			}
			return b
		}
	}
	return b
}

var (
	rules2 = []rule{
		{"ational", "ate", mGt0}, {"tional", "tion", mGt0},
		{"enci", "ence", mGt0}, {"anci", "ance", mGt0},
		{"izer", "ize", mGt0},
		{"logi", "log", mGt0},
		{"bli", "ble", mGt0}, {"alli", "al", mGt0}, {"entli", "ent", mGt0}, {"eli", "e", mGt0}, {"ousli", "ous", mGt0},
		{"ization", "ize", mGt0}, {"ation", "ate", mGt0}, {"ator", "ate", mGt0},
		{"alism", "al", mGt0}, {"iveness", "ive", mGt0}, {"fulness", "ful", mGt0}, {"ousness", "ous", mGt0},
		{"aliti", "al", mGt0}, {"iviti", "ive", mGt0}, {"biliti", "ble", mGt0},
	}
	rules3 = []rule{
		{"ical", "ic", mGt0},
		{"ness", "", mGt0},
		{"icate", "ic", mGt0}, {"iciti", "ic", mGt0},
		{"ful", "", mGt0},
		{"ative", "", mGt0},
		{"alize", "al", mGt0},
	}
	rules4 = []rule{
		{"al", "", mGt1},
		{"ance", "", mGt1}, {"ence", "", mGt1},
		{"er", "", mGt1},
		{"ic", "", mGt1},
		{"able", "", mGt1}, {"ible", "", mGt1},
		{"ant", "", mGt1}, {"ement", "", mGt1}, {"ment", "", mGt1}, {"ent", "", mGt1},
		{"ion", "", mGt1ST}, {"ou", "", mGt1},
		{"ism", "", mGt1},
		{"ate", "", mGt1}, {"iti", "", mGt1},
		{"ous", "", mGt1},
		{"ive", "", mGt1},
		{"ize", "", mGt1},
	}
)

// FTS5 picks the rules to try by a word's last letter but one, and tries them in the order
// above; no two suffixes that share that letter end the same word unless one ends the
// other, and then the longer comes first. So the first suffix a word ends with is its rule.
func step2(b []byte) []byte { return apply(b, rules2) }
func step3(b []byte) []byte { return apply(b, rules3) }
func step4(b []byte) []byte { return apply(b, rules4) }
