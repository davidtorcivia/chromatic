package handlers

import "testing"

// TestValidFileID exercises the file-id format gate used by DeleteFile to keep
// LIKE-metacharacter-bearing path values out of the chat-message cleanup query.
// generateFileID produces 32 lowercase hex chars; anything else is rejected so
// a path value like "%" can't over-match and delete unrelated file messages.
func TestValidFileID(t *testing.T) {
	valid := []string{
		"00000000000000000000000000000000", // 32 hex
		"abcdef0123456789abcdef0123456789",
		"deadbeefdeadbeefdeadbeefdeadbeef",
		generateFileID(), // a real generated id
	}
	for _, id := range valid {
		if !validFileID(id) {
			t.Errorf("validFileID(%q) = false, want true", id)
		}
	}

	invalid := []string{
		"",                  // empty
		"abc",               // too short
		"ABCDEFGHIJKLMNOP",  // uppercase (not hex-lowercase)
		"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", // not hex
		"abcdef0123456789abcdef012345678",  // 31 chars
		"abcdef0123456789abcdef0123456789a", // 33 chars
		"%",                 // LIKE wildcard
		"_",                 // LIKE single-char wildcard
		"abc%",              // hex prefix + wildcard
		"../etc/passwd",     // path traversal attempt
		"a]b",               // bracket
	}
	for _, id := range invalid {
		if validFileID(id) {
			t.Errorf("validFileID(%q) = true, want false", id)
		}
	}
}

// TestEscapeLikeForID verifies that LIKE metacharacters in a file id are escaped
// so the chat-message DELETE pattern can only match that exact id. This is the
// defence-in-depth layer behind validFileID: even if a non-hex id reached the
// LIKE, % and _ are backslash-escaped (paired with ESCAPE '\' on the clause).
func TestEscapeLikeForID(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"abcdef0123456789abcdef0123456789", "abcdef0123456789abcdef0123456789"},
		{"a%c_d", `a\%c\_d`},        // wildcard chars escaped
		{`a\b`, `a\\b`},             // backslash escaped
		{"%", `\%`},                 // bare wildcard
		{"", ""},                    // empty passes through
		{"normal", "normal"},        // plain hex-ish unchanged
	}
	for _, c := range cases {
		got := escapeLikeForID(c.in)
		if got != c.want {
			t.Errorf("escapeLikeForID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
