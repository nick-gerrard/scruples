package web

import (
	"fmt"
	"html/template"
	"strings"
	"unicode"
	"unicode/utf8"
)

var avatarPalette = []string{
	"#4f46e5", // indigo
	"#7c3aed", // violet
	"#2563eb", // blue
	"#0891b2", // cyan
	"#059669", // emerald
	"#d97706", // amber
	"#e11d48", // rose
	"#db2777", // pink
}

// avatar renders a placeholder round initials circle for the given user.
func avatar(id int64, name string, size int) template.HTML {
	if size <= 0 {
		size = 32
	}
	initial := "?"
	if trimmed := strings.TrimSpace(name); trimmed != "" {
		r, _ := utf8.DecodeRuneInString(trimmed)
		if r != utf8.RuneError {
			initial = string(unicode.ToUpper(r))
		}
	}
	idx := int(id % int64(len(avatarPalette)))
	if idx < 0 {
		idx += len(avatarPalette)
	}
	bg := avatarPalette[idx]
	fontSize := max(size*45/100, 10)
	return template.HTML(fmt.Sprintf(
		`<span title="%s" aria-hidden="true" class="inline-flex shrink-0 items-center justify-center rounded-full font-bold text-white select-none" style="width:%dpx;height:%dpx;font-size:%dpx;background-color:%s">%s</span>`,
		template.HTMLEscapeString(name),
		size, size, fontSize, bg,
		template.HTMLEscapeString(initial),
	))
}
