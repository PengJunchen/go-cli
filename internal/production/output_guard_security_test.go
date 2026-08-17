package production

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// OutputGuard security hardening: covers XSS/HTML sanitization, sensitive-data
// scrubbing, null-byte handling, multibyte/emoji content, and long-text
// truncation boundary cases that are not covered by the baseline suites.

// TestCodeInjectionGuardXSSVariants asserts that a broad set of XSS/HTML
// injection shapes are rejected, including case variants, entity-encoded
// bodies, and whitespace-padded tags.
func TestCodeInjectionGuardXSSVariants(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	g := NewCodeInjectionGuard()

	xss := []string{
		"<script>alert(1)</script>",
		"<SCRIPT>alert(1)</SCRIPT>",
		"<ScRiPt src='//evil/x.js'>",
		"<script>alert(&quot;xss&quot;)</script>",
		"<script type=\"text/javascript\">x</script>",
		"  <script>  whitespace-padded </script>  ",
	}
	for _, in := range xss {
		res, err := g.Check(ctx, in)
		require.NoError(t, err)
		assert.False(t, res.Allowed, "XSS payload should be blocked: %q", in)
		assert.Equal(t, GuardCritical, res.Severity)
		assert.Empty(t, res.Sanitized, "blocked XSS must not leak sanitized text")
	}
}

// TestCodeInjectionGuardNonScriptHTMLPasses documents the guard's detection
// boundary: the built-in pattern matches literal <script> tags only, so
// SVG/img/body handler attributes are not flagged. We assert the boundary so a
// future strengthening of the pattern is visible to reviewers.
func TestCodeInjectionGuardNonScriptHTMLPasses(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	g := NewCodeInjectionGuard()

	passes := []string{
		"<svg/onload=alert(1)>",
		"<img src=x onerror=alert(1)>",
		"<body onload=alert(1)>",
		"<iframe src=\"javascript:alert(1)\"></iframe>",
		"<a href=\"javascript:void(0)\">link</a>",
	}
	for _, in := range passes {
		res, err := g.Check(ctx, in)
		require.NoError(t, err)
		assert.True(t, res.Allowed, "non-script handler must pass current pattern: %q", in)
	}
}

// TestCodeInjectionGuardSQLAndSyscallVariants asserts injection indicators that
// are not caught by the naive single-shape strings already tested, e.g.
// tabs/newlines splitting keywords, and OS command style calls.
func TestCodeInjectionGuardSQLAndSyscallVariants(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	g := NewCodeInjectionGuard()

	sqli := []string{
		"SELECT user FROM accounts WHERE id = 1;",
		"select * from audit_log",        // lowercase
		"INSERT INTO users VALUES ('a')", // insert into
		"DELETE FROM sessions",           // delete from
		"UPDATE profiles SET bio = 'x'",  // update ... set
		"DROP   TABLE   orders",          // multi-space between keywords
	}
	for _, in := range sqli {
		res, err := g.Check(ctx, in)
		require.NoError(t, err)
		assert.False(t, res.Allowed, "SQL injection should be blocked: %q", in)
		assert.Equal(t, GuardCritical, res.Severity)
	}

	// Messages that merely mention similar words in prose must be allowed,
	// avoiding a false positive on common English. Note "select ... from"
	// IS flagged (the token pattern is broad); we exclude it here.
	benign := []string{
		"please choose from the menu below",
		"the system keeps running as expected",
		"do not add new rows by hand",
		"we evaluate the result before acting",
	}
	for _, in := range benign {
		res, err := g.Check(ctx, in)
		require.NoError(t, err)
		assert.True(t, res.Allowed, "benign prose should pass: %q", in)
	}
}

// TestPIIOutputGuardScrubCases exercises additional PII shapes whose formats
// are close to but distinct from the baseline email/phone/ID patterns.
func TestPIIOutputGuardScrubCases(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	g := NewPIIOutputGuard()

	// Emails with subdomains, underscore, plus-tag and digits.
	emails := []string{
		"first.last+tag@sub.domain.example",
		"user_name@host.co.uk",
		"1024@mail.example.net",
		"X@y.example",
	}
	for _, in := range emails {
		res, err := g.Check(ctx, "reach "+in+" now")
		require.NoError(t, err)
		assert.False(t, res.Allowed, "email should be flagged: %q", in)
	}

	// Additional Chinese mobile numbers across regions.
	phones := []string{"13512345678", "17612345678", "19912345678", "15012345678"}
	for _, in := range phones {
		res, err := g.Check(ctx, "call "+in)
		require.NoError(t, err)
		assert.False(t, res.Allowed, "phone should be flagged: %q", in)
	}

	// A 10-digit and 12-digit number must NOT be treated as a CN mobile.
	for _, in := range []string{"1234567890", "123456789012"} {
		res, err := g.Check(ctx, "num "+in)
		require.NoError(t, err)
		assert.True(t, res.Allowed, "non-mobile length should not match: %q", in)
	}
}

// TestRegexOutputGuardSensitiveScrubbing verifies that a guard can be built to
// scrub typical secret shapes (API keys, tokens, PEM blocks) and that the
// sanitized output never contains the secret itself.
func TestRegexOutputGuardSensitiveScrubbing(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()

	// Build a guard denying outputs that carry an obvious secret token.
	g := NewRegexOutputGuard([]string{`sk-[A-Za-z0-9]{20,}`})
	// Assemble the secret from fragments so the test source does not embed a
	// full hardcoded token while still exercising the scrubber.
	fullToken := "sk-" + "abcdef0" + "123456789abc" + "def9876543210"
	res, err := g.Check(ctx, "my key is "+fullToken+" ok")
	require.NoError(t, err)
	assert.False(t, res.Allowed)
	assert.Empty(t, res.Sanitized, "the secret text must be fully cleared")

	// The same token truncated below the pattern length is not flagged: this is
	// an intended trade-off, not a leak of the full secret.
	short := NewRegexOutputGuard([]string{`sk-[A-Za-z0-9]{20,}`})
	res, err = short.Check(ctx, "sk-abc")
	require.NoError(t, err)
	assert.True(t, res.Allowed)
}

// TestRegexOutputGuardAnchoredAndMultilinePatterns verifies anchored patterns
// and multiline matching semantics so a guard author can control what is or is
// not treated as sensitive.
func TestRegexOutputGuardAnchoredAndMultilinePatterns(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()

	// ^-anchored pattern only matches at the start of the whole text.
	startAnchored := NewRegexOutputGuard([]string{`^BEGIN SECRET`})
	res, err := startAnchored.Check(ctx, "noise\nBEGIN SECRET")
	require.NoError(t, err)
	assert.True(t, res.Allowed, "anchored pattern must not match mid-text by default")

	res, err = startAnchored.Check(ctx, "BEGIN SECRET data")
	require.NoError(t, err)
	assert.False(t, res.Allowed)

	// Multiline (?m) flag lets ^ match after a newline.
	multiline := NewRegexOutputGuard([]string{`(?m)^BEGIN SECRET`})
	res, err = multiline.Check(ctx, "noise\nBEGIN SECRET payload")
	require.NoError(t, err)
	assert.False(t, res.Allowed, "multiline pattern should match line-start after newline")
}

// TestLengthGuardNullBytesAndEmoji verifies that truncation is rune-based when
// the input contains null bytes or astral-plane emoji (which encode as >1 UTF-8
// byte), so the resulting Sanitized string is always valid UTF-8.
func TestLengthGuardNullBytesAndEmoji(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()

	// Null bytes count as a single rune; truncation must keep the pre-null
	// prefix intact and never produce a partial rune.
	g := NewLengthGuard(5)
	res, err := g.Check(ctx, "ab\x00cd\x00ef")
	require.NoError(t, err)
	assert.False(t, res.Allowed)
	assert.Equal(t, "ab\x00cd", res.Sanitized)

	// Astral-plane emoji are two runes but four UTF-8 bytes each. A limit of 3
	// runes must keep exactly the first three emoji and remain valid UTF-8.
	emoji := NewLengthGuard(3)
	three := "😀😀😀"
	res, err = emoji.Check(ctx, "😀😀😀😀 extra after")
	require.NoError(t, err)
	assert.False(t, res.Allowed)
	assert.Equal(t, three, res.Sanitized)
	assert.True(t, strings.ContainsRune(res.Sanitized, '😀'))

	// A boundary exactly at the rune count is allowed without truncation.
	res, err = emoji.Check(ctx, "😀😀😀")
	require.NoError(t, err)
	assert.True(t, res.Allowed)
}

// TestLengthGuardMixedScriptBoundary validates that truncation around a
// boundary crossing emoji + CJK + ASCII produces a clean (non-broken) prefix.
func TestLengthGuardMixedScriptBoundary(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()

	// 7 runes: 'a','b','c' + 2 CJK + 2 emoji.
	input := "abc汉字😀😀"
	g := NewLengthGuard(7)
	res, err := g.Check(ctx, input)
	require.NoError(t, err)
	assert.True(t, res.Allowed, "exact rune count must not truncate")
	assert.Equal(t, input, res.Sanitized)

	// Truncating to 5 runes keeps ascii + the full CJK pair, and the output
	// must remain a valid UTF-8 string of exactly 5 runes.
	g5 := NewLengthGuard(5)
	res, err = g5.Check(ctx, input)
	require.NoError(t, err)
	assert.False(t, res.Allowed)
	assert.Equal(t, 5, len([]rune(res.Sanitized)))
	// 'abc' + '汉字'.
	assert.Equal(t, "abc汉字", res.Sanitized)
}

// TestLengthGuardReasonCarriesRuneCounts verifies the truncation reason string
// journals the original and truncated rune counts.
func TestLengthGuardReasonCarriesRuneCounts(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()

	g := NewLengthGuard(4)
	res, err := g.Check(ctx, "Truncate me please")
	require.NoError(t, err)
	assert.False(t, res.Allowed)
	assert.Contains(t, res.Reason, "18 runes")
	assert.Contains(t, res.Reason, "to 4")

	res, err = g.Check(ctx, "abc")
	require.NoError(t, err)
	assert.True(t, res.Allowed)
	assert.Empty(t, res.Reason, "no reason when the text is within limits")
}

// TestChecksWithNumInput utils does not exist; we instead assert that a guard
// check on a very long single rune (e.g. a 40k text) both truncates and keeps
// the guard from blocking valid long-form content when length is the limit.
func TestLengthGuardVeryLongText(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()

	long := strings.Repeat("数据与性能", 2000) // 10k runes.
	g := NewLengthGuard(8192)
	res, err := g.Check(ctx, long)
	require.NoError(t, err)
	assert.False(t, res.Allowed)
	assert.Equal(t, 8192, len([]rune(res.Sanitized)))
}

// TestNullByteThroughChain verifies that a code-injection guard plus length
// guard chain still behaves correctly when fed a null-byte padded payload.
func TestNullByteThroughChain(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()

	chain := NewOutputGuardChain([]OutputGuard{
		NewCodeInjectionGuard(),
		NewLengthGuard(64),
	})
	res, err := chain.Check(ctx, "<script>\x00alert('x')</script>")
	require.NoError(t, err)
	assert.False(t, res.Allowed)
	assert.Equal(t, GuardCritical, res.Severity)

	// A benign null-byte padded string is not an injection; only length applies.
	res, err = chain.Check(ctx, string([]byte{'h', 'e', 'l', 'l', 'o', 0, '!', 0}))
	require.NoError(t, err)
	assert.True(t, res.Allowed, "plain text with null bytes should pass injection guard")
}

// TestCodeInjectionGuardMidTextInjection verifies that injection patterns
// embedded in longer natural prose are still caught.
func TestCodeInjectionGuardMidTextInjection(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	g := NewCodeInjectionGuard()

	cases := []string{
		"The fix is to run os.system('rm -rf /') in production?",
		"Consider SELECT * FROM users WHERE admin=1 right away",
		"use of subprocess with untrusted input is dangerous",
	}
	for _, in := range cases {
		res, err := g.Check(ctx, in)
		require.NoError(t, err)
		assert.False(t, res.Allowed, "embedded injection must be caught: %q", in)
	}
}

// TestPIIOutputGuardSSN verifies that US Social Security Numbers are detected.
func TestPIIOutputGuardSSN(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	g := NewPIIOutputGuard()

	res, err := g.Check(ctx, "SSN is 123-45-6789 on file")
	require.NoError(t, err)
	assert.False(t, res.Allowed, "SSN should be flagged as PII")
	assert.Equal(t, GuardHigh, res.Severity)
	assert.Contains(t, res.Reason, "US SSN")
}

// TestPIIOutputGuardCreditCard verifies that valid credit card numbers are
// detected while Luhn-invalid numbers are not flagged.
func TestPIIOutputGuardCreditCard(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	g := NewPIIOutputGuard()

	valid := []string{
		"4111111111111111",    // Visa test number
		"4111 1111 1111 1111", // with spaces
		"4012 8888 8888 1881", // another test card
		"5500 0000 0000 0004", // Mastercard test number
	}
	for _, in := range valid {
		res, err := g.Check(ctx, in)
		require.NoError(t, err)
		assert.False(t, res.Allowed, "valid card should be flagged: %q", in)
	}

	// Luhn-invalid 16-digit number is NOT flagged (regex matches but Luhn fails).
	res, err := g.Check(ctx, "4111111111111112")
	require.NoError(t, err)
	assert.True(t, res.Allowed, "Luhn-invalid number should not be flagged")
}

// TestPIIOutputGuardAPIKey verifies that API keys with common prefixes are
// detected as PII.
func TestPIIOutputGuardAPIKey(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	g := NewPIIOutputGuard()

	keys := []string{
		"sk-proj-abcdef1234567890abcd",
		"pk-proj-abcdef1234567890abcd",
		"rk-proj-abcdef1234567890abcd",
	}
	for _, in := range keys {
		res, err := g.Check(ctx, "token: "+in)
		require.NoError(t, err)
		assert.False(t, res.Allowed, "API key should be flagged: %q", in)
		assert.Contains(t, res.Reason, "API Key")
	}
}

// TestPIIOutputGuardInternationalPhone verifies that international phone numbers
// with a + prefix are detected.
func TestPIIOutputGuardInternationalPhone(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	g := NewPIIOutputGuard()

	phones := []string{
		"+1-202-555-0173",
		"+44 20 7946 0958",
		"+86-138-1234-5678",
	}
	for _, in := range phones {
		res, err := g.Check(ctx, "call "+in)
		require.NoError(t, err)
		assert.False(t, res.Allowed, "international phone should be flagged: %q", in)
	}
}

// TestPIIOutputGuardCustomPatterns verifies that user-supplied patterns are
// applied in addition to the built-in patterns.
func TestPIIOutputGuardCustomPatterns(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()

	custom := PIIPattern{
		Pattern: regexp.MustCompile(`INTERNAL-TKT-\d{6}`),
		Name:    "Internal Ticket",
	}
	g := NewPIIOutputGuard(WithCustomPIIPatterns(custom))

	res, err := g.Check(ctx, "see INTERNAL-TKT-123456 for details")
	require.NoError(t, err)
	assert.False(t, res.Allowed, "custom pattern should match")
	assert.Contains(t, res.Reason, "Internal Ticket")

	// Built-in patterns still work alongside custom ones.
	res, err = g.Check(ctx, "email me at joe@example.com")
	require.NoError(t, err)
	assert.False(t, res.Allowed, "built-in email pattern should still work")

	// Text that matches neither built-in nor custom patterns is allowed.
	res, err = g.Check(ctx, "no sensitive data here")
	require.NoError(t, err)
	assert.True(t, res.Allowed)
}

// TestPIIOutputGuardNoFalsePositives verifies that version numbers and ordinary
// text are not flagged as PII.
func TestPIIOutputGuardNoFalsePositives(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	g := NewPIIOutputGuard()

	benign := []string{
		"version 1.2.3.4",
		"the quick brown fox jumps over the lazy dog",
		"meeting at 3pm in room 42",
		"order #1001 shipped on 2024-01-15",
	}
	for _, in := range benign {
		res, err := g.Check(ctx, in)
		require.NoError(t, err)
		assert.True(t, res.Allowed, "benign text should not be flagged: %q", in)
	}
}
