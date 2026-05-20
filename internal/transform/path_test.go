package transform

import "testing"

func TestParsePathAcceptsValidGrammar(t *testing.T) {
	cases := []struct {
		in   string
		want Path
	}{
		{"text", Path{{Name: "text"}}},
		{"data.user.email", Path{{Name: "data"}, {Name: "user"}, {Name: "email"}}},
		{"data[].createdAt", Path{{Name: "data", Array: true}, {Name: "createdAt"}}},
		{"groups[].members[].id", Path{
			{Name: "groups", Array: true},
			{Name: "members", Array: true},
			{Name: "id"},
		}},
	}
	for _, tc := range cases {
		got, err := ParsePath(tc.in)
		if err != nil {
			t.Errorf("ParsePath(%q): unexpected error %v", tc.in, err)
			continue
		}
		if !got.Equal(tc.want) {
			t.Errorf("ParsePath(%q) = %v, want %v", tc.in, got, tc.want)
		}
		if got.String() != tc.in {
			t.Errorf("ParsePath(%q).String() = %q, want %q", tc.in, got.String(), tc.in)
		}
	}
}

func TestParsePathRejectsInvalidGrammar(t *testing.T) {
	cases := []string{
		"",
		".",
		"a.",
		".a",
		"a..b",
		"a[].",
		"a[0]",
		"a[].b[",
		"a.b]",
		"a]",
		"a.[]",
		"[]",
		"a[]", // terminal [] — paths must end on a Name leaf
	}
	for _, in := range cases {
		if _, err := ParsePath(in); err == nil {
			t.Errorf("ParsePath(%q): expected error, got nil", in)
		}
	}
}

func TestPathEqualAndPrefix(t *testing.T) {
	p, _ := ParsePath("data.user.email")
	prefixCases := []struct {
		prefix string
		want   bool
	}{
		{"data", true},
		{"data.user", true},
		{"data.user.email", true},
		{"data.user.name", false},
		{"data.x", false},
		{"foo", false},
	}
	for _, tc := range prefixCases {
		pre, _ := ParsePath(tc.prefix)
		if got := p.HasPrefixByName(pre); got != tc.want {
			t.Errorf("Path(%q).HasPrefixByName(%q) = %v, want %v", p, pre, got, tc.want)
		}
	}

	// Prefix matching ignores Array-ness on segments.
	pa, _ := ParsePath("data[].user.email")
	dataNoArr, _ := ParsePath("data")
	if !pa.HasPrefixByName(dataNoArr) {
		t.Errorf("data[].user.email should be prefix-matched by 'data' (Array flag ignored)")
	}

	// Converse: prefix has Array=true but path doesn't — still matches by name only.
	prefixArrayed, _ := ParsePath("data[].user")
	pNoArr2, _ := ParsePath("data.user")
	if !pNoArr2.HasPrefixByName(prefixArrayed) {
		t.Errorf("HasPrefixByName must ignore Array on the prefix too (data[].user prefix of data.user)")
	}

	// Equal is strict on Array
	pNoArr, _ := ParsePath("data")
	if pNoArr.Equal(Path{{Name: "data", Array: true}}) {
		t.Errorf("Path.Equal must distinguish Array flag")
	}
}
