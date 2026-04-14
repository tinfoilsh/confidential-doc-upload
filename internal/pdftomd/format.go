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

// IsBullet returns true if the rune is a bullet-like character.
func IsBullet(r rune) bool {
	if r >= 0x25A0 && r <= 0x25FF {
		return true
	}
	return bulletChars[r]
}

// Span represents a run of text with uniform style.
type Span struct {
	Text   string
	Bold   bool
	Italic bool
	Mono   bool
	Size   float64
	X0     float64
}

// FormatSpan wraps span text with markdown style markers.
func FormatSpan(s Span) string {
	text := strings.TrimRight(s.Text, " ")
	if text == "" {
		return ""
	}

	if s.Mono {
		text = "`" + text + "`"
	}
	if s.Bold {
		text = "**" + text + "**"
	}
	if s.Italic {
		text = "_" + text + "_"
	}
	return text
}

// FormatBulletLine converts a bullet-prefixed line to markdown list syntax
// with appropriate indentation based on x-offset.
func FormatBulletLine(text string, xOffset float64, clipX0 float64, charWidth float64) string {
	if len(text) < 2 {
		return text
	}
	rest := strings.TrimLeft(text[1:], " ")
	mdText := "- " + rest

	if charWidth <= 0 {
		charWidth = 6
	}
	indent := int((xOffset - clipX0) / charWidth)
	if indent < 0 {
		indent = 0
	}
	return strings.Repeat(" ", indent) + mdText
}
