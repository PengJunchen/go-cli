package tools

import (
	"fmt"
	"regexp"
	"strings"
)

// HTMLToMarkdownConverter converts HTML content to Markdown.
type HTMLToMarkdownConverter interface {
	Convert(html string) (string, error)
}

// DefaultHTMLConverterOption configures a DefaultHTMLConverter.
type DefaultHTMLConverterOption func(*DefaultHTMLConverter)

// WithMaxLines caps the converted output at maxLines lines. A value of 0 means
// unlimited. Truncation avoids breaking inside a code block.
func WithMaxLines(n int) DefaultHTMLConverterOption {
	return func(c *DefaultHTMLConverter) { c.maxLines = n }
}

// DefaultHTMLConverter uses standard library regexp + strings for conversion.
// It has zero third-party dependencies.
type DefaultHTMLConverter struct {
	maxLines int  // max lines of output, 0 = unlimited
	color    bool // ANSI color output (not used in default, for future)
}

var _ HTMLToMarkdownConverter = (*DefaultHTMLConverter)(nil)

// NewDefaultHTMLConverter returns a DefaultHTMLConverter configured by opts.
func NewDefaultHTMLConverter(opts ...DefaultHTMLConverterOption) *DefaultHTMLConverter {
	c := &DefaultHTMLConverter{}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Precompiled patterns. RE2 (Go regexp) is not backtracking, so each pattern
// is independent. All patterns are case-insensitive and dotall (s flag) so they
// span multiple lines and match tags regardless of case.
var (
	// blockRemovalRes match entire script/style/nav/footer/header elements with
	// their content so they can be stripped before content extraction.
	blockRemovalRes = []*regexp.Regexp{
		regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script>`),
		regexp.MustCompile(`(?is)<style\b[^>]*>.*?</style>`),
		regexp.MustCompile(`(?is)<nav\b[^>]*>.*?</nav>`),
		regexp.MustCompile(`(?is)<footer\b[^>]*>.*?</footer>`),
		regexp.MustCompile(`(?is)<header\b[^>]*>.*?</header>`),
	}
	// mainContentRe extracts the inner content of the highest-priority container.
	articleRe   = regexp.MustCompile(`(?is)<article\b[^>]*>(.*?)</article>`)
	mainTagRe   = regexp.MustCompile(`(?is)<main\b[^>]*>(.*?)</main>`)
	bodyRe      = regexp.MustCompile(`(?is)<body\b[^>]*>(.*?)</body>`)
	preCodeRe   = regexp.MustCompile(`(?is)<pre\b[^>]*>\s*<code\b[^>]*>(.*?)</code>\s*</pre>`)
	codeRe      = regexp.MustCompile(`(?is)<code\b[^>]*>(.*?)</code>`)
	headingRe   = regexp.MustCompile(`(?is)<h([1-6])\b[^>]*>(.*?)</h[1-6]\s*>`)
	linkRe      = regexp.MustCompile(`(?is)<a\b[^>]*href=["']([^"']*)["'][^>]*>(.*?)</a>`)
	ulRe        = regexp.MustCompile(`(?is)<ul\b[^>]*>(.*?)</ul>`)
	olRe        = regexp.MustCompile(`(?is)<ol\b[^>]*>(.*?)</ol>`)
	liRe        = regexp.MustCompile(`(?is)<li\b[^>]*>(.*?)</li>`)
	blockquote  = regexp.MustCompile(`(?is)<blockquote\b[^>]*>(.*?)</blockquote>`)
	pTagRe      = regexp.MustCompile(`(?is)<p\b[^>]*>(.*?)</p>`)
	strongRe    = regexp.MustCompile(`(?is)<strong\b[^>]*>(.*?)</strong>`)
	bRe         = regexp.MustCompile(`(?is)<b\b[^>]*>(.*?)</b>`)
	emRe        = regexp.MustCompile(`(?is)<em\b[^>]*>(.*?)</em>`)
	iRe         = regexp.MustCompile(`(?is)<i\b[^>]*>(.*?)</i>`)
	brRe        = regexp.MustCompile(`(?i)<br\s*/?>`)
	hrRe        = regexp.MustCompile(`(?i)<hr\s*/?>`)
	stripTagRe  = regexp.MustCompile(`<[^>]*>`)
	multiNewRe  = regexp.MustCompile(`\n{3,}`)
	codeFenceRe = regexp.MustCompile("^```")
)

// Convert transforms HTML into Markdown using only the standard library.
func (c *DefaultHTMLConverter) Convert(html string) (string, error) {
	s := html

	// 1. Remove script/style/nav/footer/header blocks entirely.
	for _, re := range blockRemovalRes {
		s = re.ReplaceAllString(s, "")
	}

	// 2. Extract main content in priority order: article > main > body.
	s = extractMainContent(s)

	// 3. Convert HTML tags to Markdown.
	s = preCodeRe.ReplaceAllString(s, "```\n$1\n```")
	s = codeRe.ReplaceAllString(s, "`$1`")
	s = convertHeadings(s)
	s = linkRe.ReplaceAllString(s, "[$2]($1)")
	s = convertLists(s)
	s = convertBlockquotes(s)
	s = pTagRe.ReplaceAllString(s, "\n\n$1\n\n")
	s = strongRe.ReplaceAllString(s, "**$1**")
	s = bRe.ReplaceAllString(s, "**$1**")
	s = emRe.ReplaceAllString(s, "*$1*")
	s = iRe.ReplaceAllString(s, "*$1*")
	s = brRe.ReplaceAllString(s, "\n")
	s = hrRe.ReplaceAllString(s, "\n---\n")

	// 4. Strip remaining HTML tags.
	s = stripTagRe.ReplaceAllString(s, "")

	// 5. Clean up extra whitespace (collapse multiple blank lines to max 2).
	s = cleanWhitespace(s)

	// 6. Truncate to maxLines without breaking inside a code block.
	if c.maxLines > 0 {
		s = truncateLines(s, c.maxLines)
	}
	return s, nil
}

// extractMainContent returns the inner HTML of the first container found in
// priority order: <article>, <main>, <body>. If none match the input is
// returned unchanged.
func extractMainContent(s string) string {
	if m := articleRe.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	if m := mainTagRe.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	if m := bodyRe.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return s
}

func convertHeadings(s string) string {
	return headingRe.ReplaceAllStringFunc(s, func(match string) string {
		sub := headingRe.FindStringSubmatch(match)
		level := int(sub[1][0] - '0')
		return strings.Repeat("#", level) + " " + strings.TrimSpace(sub[2]) + "\n"
	})
}

func convertLists(s string) string {
	s = ulRe.ReplaceAllStringFunc(s, func(block string) string {
		inner := ulRe.FindStringSubmatch(block)[1]
		return liRe.ReplaceAllStringFunc(inner, func(li string) string {
			content := liRe.FindStringSubmatch(li)[1]
			return "- " + strings.TrimSpace(content) + "\n"
		})
	})
	s = olRe.ReplaceAllStringFunc(s, func(block string) string {
		inner := olRe.FindStringSubmatch(block)[1]
		i := 0
		return liRe.ReplaceAllStringFunc(inner, func(li string) string {
			i++
			content := liRe.FindStringSubmatch(li)[1]
			return fmt.Sprintf("%d. %s\n", i, strings.TrimSpace(content))
		})
	})
	return s
}

func convertBlockquotes(s string) string {
	return blockquote.ReplaceAllStringFunc(s, func(block string) string {
		inner := strings.TrimSpace(blockquote.FindStringSubmatch(block)[1])
		lines := strings.Split(inner, "\n")
		for i, l := range lines {
			lines[i] = "> " + strings.TrimSpace(l)
		}
		return strings.Join(lines, "\n") + "\n"
	})
}

func cleanWhitespace(s string) string {
	s = strings.TrimSpace(s)
	s = multiNewRe.ReplaceAllString(s, "\n\n")
	return s
}

// truncateLines caps the output at max lines. If truncation would leave an
// unclosed code block, the incomplete block is dropped entirely so the output
// never contains a dangling fence.
func truncateLines(s string, max int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= max {
		return s
	}
	truncated := lines[:max]

	// Count code fences in the kept portion; an odd count means we are inside
	// an unfinished code block.
	fences := 0
	for _, l := range truncated {
		if codeFenceRe.MatchString(strings.TrimSpace(l)) {
			fences++
		}
	}
	if fences%2 != 0 {
		// Drop from the last opening fence onward.
		for i := len(truncated) - 1; i >= 0; i-- {
			if codeFenceRe.MatchString(strings.TrimSpace(truncated[i])) {
				truncated = truncated[:i]
				break
			}
		}
	}
	return strings.Join(truncated, "\n")
}
