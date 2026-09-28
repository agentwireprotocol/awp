package main

import (
	"slices"
	"testing"
)

func sendFlags() *flags {
	f := newFlags("send", "", "")
	f.StringP("thread", "t", "", "")
	f.String("to", "", "")
	return f
}

func TestKeyPrefixArgs(t *testing.T) {
	const m = keyMark
	for _, c := range []struct{ in, want []string }{
		{[]string{"-EdZIWymh9"}, []string{m + "-EdZIWymh9"}},
		{[]string{"-EdZIWymh9", "hi"}, []string{m + "-EdZIWymh9", "hi"}},
		{[]string{"--json", "-EdZIWymh9", "name"}, []string{"--json", m + "-EdZIWymh9", "name"}},
		{[]string{"-EdZIWymh9", "hi", "--json"}, []string{m + "-EdZIWymh9", "hi", "--json"}},
		{[]string{"-t", "thr_x", "-EdZIWymh9", "hi"}, []string{"-t", "thr_x", m + "-EdZIWymh9", "hi"}},
		{[]string{"-t=thr_x", "-EdZIWymh9"}, []string{"-t=thr_x", m + "-EdZIWymh9"}},
		{[]string{"-EdZIWymh9", "-K7LtjQDxY"}, []string{m + "-EdZIWymh9", m + "-K7LtjQDxY"}},
		// Left alone: a flag's value, a defined flag or shorthand, a long
		// flag, an explicit --, and strings too short or with =.
		{[]string{"--to", "-EdZIWymh9", "hi"}, nil},
		{[]string{"-t", "-EdZIWymh9"}, nil},
		{[]string{"-tEdZIWymh9"}, nil},
		{[]string{"-thread"}, nil},
		{[]string{"--thread"}, nil},
		{[]string{"--", "-EdZIWymh9"}, nil},
		{[]string{"-EdZ"}, nil},
		{[]string{"-EdZ=IWymh9"}, nil},
		{[]string{"-"}, nil},
	} {
		want := c.want
		if want == nil {
			want = c.in
		}
		if got := keyPrefixArgs(sendFlags().FlagSet, c.in); !slices.Equal(got, want) {
			t.Errorf("keyPrefixArgs(%q) = %q, want %q", c.in, got, want)
		}
	}
}

// A key prefix that starts with - parses as a positional for connect, send
// and alias alike, and the flags around it still count.
func TestParseKeyPrefix(t *testing.T) {
	for _, c := range []struct {
		args, want []string
		json       bool
	}{
		{[]string{"-EdZIWymh9"}, []string{"-EdZIWymh9"}, false},
		{[]string{"-EdZIWymh9", "hi"}, []string{"-EdZIWymh9", "hi"}, false},
		{[]string{"-EdZIWymh9", "name"}, []string{"-EdZIWymh9", "name"}, false},
		{[]string{"--json", "-EdZIWymh9", "hi there"}, []string{"-EdZIWymh9", "hi there"}, true},
		{[]string{"-EdZIWymh9", "hi there", "--json"}, []string{"-EdZIWymh9", "hi there"}, true},
	} {
		f := sendFlags()
		if err := f.Parse(c.args); err != nil {
			t.Fatalf("Parse(%q): %v", c.args, err)
		}
		if !slices.Equal(f.Args(), c.want) || f.Arg(0) != c.want[0] || *f.json != c.json {
			t.Errorf("Parse(%q): positionals %q (want %q), json %v (want %v)", c.args, f.Args(), c.want, *f.json, c.json)
		}
	}
	f := sendFlags()
	if err := f.Parse([]string{"-t", "-EdZIWymh9", "hi"}); err != nil {
		t.Fatal(err)
	}
	if th, _ := f.GetString("thread"); th != "-EdZIWymh9" || !slices.Equal(f.Args(), []string{"hi"}) {
		t.Errorf("-t kept its value? thread %q, positionals %q", th, f.Args())
	}
}
