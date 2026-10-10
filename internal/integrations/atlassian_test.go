package integrations

import (
	"strings"
	"testing"
)

func TestMarkdownToStorage(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"heading", "# Title", "<h1>Title</h1>"},
		{"bullets", "- a\n- b", "<ul><li>a</li><li>b</li></ul>"},
		{"rule", "text\n\n---\n\nmore", "<p>text</p><hr/><p>more</p>"},
		{"italic", "*Published by the orchestrator.*", "<p><em>Published by the orchestrator.</em></p>"},
		{"bold and code", "a **b** and `c`", "<p>a <strong>b</strong> and <code>c</code></p>"},
		{"escapes html", "x < y & z", "<p>x &lt; y &amp; z</p>"},
		{"math is not italic", "2 * 3 * 4", "<p>2 * 3 * 4</p>"},
	}
	for _, c := range cases {
		if got := MarkdownToStorage(c.in); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
	if got := MarkdownToStorage("```\nfmt.Println(1)\n```"); !strings.Contains(got, `ac:name="code"`) || !strings.Contains(got, "fmt.Println(1)") {
		t.Errorf("code block not converted: %q", got)
	}
}
