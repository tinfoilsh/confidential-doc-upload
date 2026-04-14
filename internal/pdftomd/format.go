package pdftomd

import "strings"

var bulletChars = map[rune]bool{
	'•': true, '·': true, '‣': true, '▪': true, '▸': true, '▹': true,
	'►': true, '●': true, '○': true, '◦': true, '◆': true, '◇': true,
	'■': true, '□': true, '★': true, '☆': true, '➤': true, '➢': true,
	'‐': true, '‑': true, '‒': true, '–': true, '—': true, '―': true,
	'¶': true, '†': true, '‡': true, '※': true,
	'\uf0a7': true, '\uf0b7': true, '\ufffd': true,
}

func IsBullet(r rune) bool {
	if r >= 0x25A0 && r <= 0x25FF {
		return true
	}
	return bulletChars[r]
}

// Span represents a run of text with uniform style.
type Span struct {
	Text        string
	Bold        bool
	Italic      bool
	Mono        bool
	Strikeout   bool
	Superscript bool
	Size        float64
	X0          float64
}

// FormatSpan wraps span text with markdown style markers.
// Nesting order matches pymupdf4llm: strikeout > italic > bold > mono (outside to inside).
func FormatSpan(s Span) string {
	text := strings.TrimRight(s.Text, " ")
	if text == "" {
		return ""
	}

	// Handle superscripts: wrap in brackets like pymupdf4llm
	if s.Superscript {
		text = "[" + text + "]"
	}

	prefix := ""
	suffix := ""

	// Nesting order (outside to inside): strikeout, italic, bold, mono
	if s.Mono {
		prefix = "`" + prefix
		suffix += "`"
	}
	if s.Bold {
		prefix = "**" + prefix
		suffix += "**"
	}
	if s.Italic {
		prefix = "_" + prefix
		suffix += "_"
	}
	if s.Strikeout {
		prefix = "~~" + prefix
		suffix += "~~"
	}

	return prefix + text + suffix
}

// FormatBulletLine converts a bullet-prefixed line to markdown list syntax.
func FormatBulletLine(text string, xOffset float64, clipX0 float64, charWidth float64) string {
	if len(text) < 2 {
		return text
	}
	rest := strings.TrimLeft(text[1:], " ")
	mdText := "- " + rest
	mdText = strings.ReplaceAll(mdText, "  ", " ")

	if charWidth <= 0 {
		charWidth = 6
	}
	indent := int((xOffset - clipX0) / charWidth)
	if indent < 0 {
		indent = 0
	}
	return strings.Repeat(" ", indent) + mdText
}
