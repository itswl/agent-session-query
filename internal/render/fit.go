package render

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/itswl/agent-session-query/internal/index"
	"github.com/itswl/agent-session-query/internal/model"
)

// An answer held to a budget — what an agent reads is paid for by the token — leaves out
// whole parts it can ask for again, and says how: the MCP tools in their arguments, the
// command line in its flags (Style.Args).

// FitOutline prints a session's outline in about budget characters. One too long keeps its
// latest turns, where the work stands, and says how to read the ones before. A budget of
// zero is no bound.
func FitOutline(w io.Writer, s *index.Session, turns []model.Turn, children []index.Session, budget int, st Style) {
	draw := func(turns []model.Turn) string {
		var b bytes.Buffer
		Outline(&b, s, turns, children, st)
		return b.String()
	}
	text := draw(turns)
	kept := turns
	for budget > 0 && len(text) > budget && len(kept) > 4 {
		kept = kept[len(kept)-max(len(kept)*budget/len(text)*9/10, 4):]
		text = draw(kept)
	}
	if len(kept) < len(turns) && len(kept) > 0 {
		first := kept[0].Index
		if st.Args {
			text += fmt.Sprintf("\n[Turns before #%d left out: read with turns=\"1-%d\" to see them step by step, or raise max_chars for the whole outline.]\n", first, first-1)
		} else {
			text += fmt.Sprintf("\n[Turns before #%d left out: --turns 1-%d shows them step by step; raise --max-chars for the whole outline.]\n", first, first-1)
		}
	}
	io.WriteString(w, text)
}

// FitTurns prints turns step by step in about budget characters, spent turn by turn, so
// that what is left out is whole turns that can be asked for by number and not the tail of
// one. A budget of zero is no bound.
func FitTurns(w io.Writer, s *index.Session, turns []model.Turn, full bool, budget int, st Style) {
	var b bytes.Buffer
	left := budget
	for i := range turns {
		if i > 0 {
			if budget > 0 && left <= 0 {
				a, z := turns[i].Index, turns[len(turns)-1].Index
				if st.Args {
					fmt.Fprintf(&b, "\n[Turns #%d–#%d left out: read them with turns=\"%d-%d\".]\n", a, z, a, z)
				} else {
					fmt.Fprintf(&b, "\n[Turns #%d–#%d left out: --turns %d-%d shows them.]\n", a, z, a, z)
				}
				break
			}
			b.WriteByte('\n')
		}
		before := b.Len()
		o := TurnOptions{Full: full}
		if budget > 0 {
			o.MaxChars = max(left, 1)
		}
		Turn(&b, s, &turns[i], o, st)
		left -= b.Len() - before
	}
	w.Write(b.Bytes())
}

// Cut holds text to about budget characters, on a line boundary, and says how much more
// there was and what to do about it.
func Cut(text string, budget int, advice string) string {
	if budget <= 0 || len(text) <= budget {
		return text
	}
	at := budget
	for at > 0 && !utf8.RuneStart(text[at]) {
		at--
	}
	if nl := strings.LastIndexByte(text[:at], '\n'); nl > budget/2 {
		at = nl
	}
	return text[:at] + fmt.Sprintf("\n\n[Cut here: %d more characters. %s]", len(text)-at, advice)
}
