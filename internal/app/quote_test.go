package app

import "testing"

func TestQuoteArg(t *testing.T) {
	for in, want := range map[string][2]string{
		"3f2a9c1e-0000-4000-8000-000000000001": {"3f2a9c1e-0000-4000-8000-000000000001", "3f2a9c1e-0000-4000-8000-000000000001"},
		"/work/proj":                           {"/work/proj", "/work/proj"},
		"/work/my project":                     {"'/work/my project'", `"/work/my project"`},
		`C:\Users\me\proj`:                     {`'C:\Users\me\proj'`, `C:\Users\me\proj`},
		`C:\Users\my name\proj`:                {`'C:\Users\my name\proj'`, `"C:\Users\my name\proj"`},
		"it's":                                 {`'it'\''s'`, `"it's"`},
		`say "hi"`:                             {`'say "hi"'`, `"say ""hi"""`},
		"a;rm -rf ~":                           {"'a;rm -rf ~'", `"a;rm -rf ~"`},
		"$(reboot)":                            {"'$(reboot)'", `"$(reboot)"`},
		"a&calc":                               {"'a&calc'", `"a&calc"`},
		"":                                     {"''", `""`},
	} {
		if got := quoteArg(in, false); got != want[0] {
			t.Errorf("sh: quoteArg(%q) = %s, want %s", in, got, want[0])
		}
		if got := quoteArg(in, true); got != want[1] {
			t.Errorf("cmd: quoteArg(%q) = %s, want %s", in, got, want[1])
		}
	}
}
