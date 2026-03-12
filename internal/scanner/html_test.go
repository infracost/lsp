package scanner

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHtmlToMarkdown(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "simple anchor tag",
			in:   `<a href="https://example.com">link text</a>`,
			want: "[link text](https://example.com)",
		},
		{
			name: "anchor with target blank",
			in:   `<a href="https://example.com" target="_blank">link</a>`,
			want: "[link](https://example.com)",
		},
		{
			name: "multiple anchors",
			in:   `See <a href="https://one.com">first</a> and <a href="https://two.com">second</a>.`,
			want: "See [first](https://one.com) and [second](https://two.com).",
		},
		{
			name: "non-anchor HTML tags stripped",
			in:   `Hello<br>world<div>content</div>`,
			want: "Helloworldcontent",
		},
		{
			name: "no HTML passes through unchanged",
			in:   "plain text with no tags",
			want: "plain text with no tags",
		},
		{
			name: "mixed anchors and other HTML",
			in:   `<b>Bold</b> and <a href="https://example.com">link</a><br>done`,
			want: "**Bold** and [link](https://example.com)done",
		},
		{
			name: "Italic tags",
			in:   `This is <i>italic</i> and <em>emphasized</em>.`,
			want: "This is _italic_ and _emphasized_.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := htmlToMarkdown(tt.in)
			assert.Equal(t, tt.want, got)
		})
	}
}
