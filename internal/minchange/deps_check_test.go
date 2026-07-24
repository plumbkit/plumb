package minchange

import "testing"

func TestQuotedString_StopsAtUnescapedQuote(t *testing.T) {
	val, rest, ok := quotedString(`"foo" , "next"`)
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	if val != "foo" {
		t.Errorf("val = %q, want foo", val)
	}
	if rest != ` , "next"` {
		t.Errorf("rest = %q", rest)
	}
}

func TestQuotedString_EscapedQuoteDoesNotTruncateValue(t *testing.T) {
	// A naive scan for the first raw '"' byte stops at the escaped \" inside
	// the value, truncating it and leaving the rest of the value ("bar\" rest)
	// as unconsumed "rest" text. The fix must find the real closing quote and
	// decode the escape.
	val, rest, ok := quotedString(`"foo\"bar" rest`)
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	if val != `foo"bar` {
		t.Errorf(`val = %q, want foo"bar`, val)
	}
	if rest != " rest" {
		t.Errorf("rest = %q, want %q", rest, " rest")
	}
}

func TestQuotedString_UnterminatedIsNotOK(t *testing.T) {
	if _, _, ok := quotedString(`"unterminated`); ok {
		t.Errorf("ok = true, want false for an unterminated quoted string")
	}
}

func TestJSONDepKey_ValueWithEscapedQuote(t *testing.T) {
	// A package.json dependency line whose version value contains an escaped
	// quote must still resolve to the correct key rather than being corrupted
	// by the closing-quote scan.
	got := jsonDepKey(`  "left-pad": "1.3.0 \"stable\""`)
	if got != "left-pad" {
		t.Errorf("jsonDepKey = %q, want left-pad", got)
	}
}
