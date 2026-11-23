package web

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"html/template"
	"strings"
)

// Avatar renders an <img> tag for the user's avatar at the given pixel size.
func Avatar(id int64, name string, size int) template.HTML {
	if size <= 0 {
		size = 32
	}
	return template.HTML(fmt.Sprintf(
		`<img src="/avatar/%d" alt="%s" width="%d" height="%d" loading="lazy" class="inline-block shrink-0 rounded-full object-cover" style="width:%dpx;height:%dpx">`,
		id, template.HTMLEscapeString(name), size, size, size, size,
	))
}

type avatarPalette struct {
	bg      string
	bgShade string
	face    string
	accent  string
	ink     string
	light   string
}

// Curated 3-color palettes designed to read clearly on dark slate backgrounds.
var avatarPalettes = []avatarPalette{
	{bg: "#312e81", bgShade: "#3730a3", face: "#818cf8", accent: "#38bdf8", ink: "#0f172a", light: "#eef2ff"},
	{bg: "#134e4a", bgShade: "#115e59", face: "#2dd4bf", accent: "#fde047", ink: "#0f172a", light: "#f0fdfa"},
	{bg: "#881337", bgShade: "#9f1239", face: "#fb7185", accent: "#fbbf24", ink: "#0f172a", light: "#fff1f2"},
	{bg: "#78350f", bgShade: "#92400e", face: "#fbbf24", accent: "#fb7185", ink: "#0f172a", light: "#fffbeb"},
	{bg: "#4c1d95", bgShade: "#5b21b6", face: "#c084fc", accent: "#f472b6", ink: "#0f172a", light: "#faf5ff"},
	{bg: "#0c4a6e", bgShade: "#075985", face: "#38bdf8", accent: "#a78bfa", ink: "#0f172a", light: "#f0f9ff"},
	{bg: "#064e3b", bgShade: "#065f46", face: "#34d399", accent: "#facc15", ink: "#0f172a", light: "#ecfdf5"},
	{bg: "#7c2d12", bgShade: "#9a3412", face: "#fb923c", accent: "#38bdf8", ink: "#0f172a", light: "#fff7ed"},
	{bg: "#1e3a5f", bgShade: "#1d4ed8", face: "#60a5fa", accent: "#fbbf24", ink: "#0f172a", light: "#eff6ff"},
	{bg: "#581c87", bgShade: "#6b21a8", face: "#e879f9", accent: "#2dd4bf", ink: "#0f172a", light: "#fdf4ff"},
}

// GenerateAvatarSVG builds a deterministic, friendly geometric face SVG for a
// user from their id and username. Unknown users (empty username) still get a
// deterministic avatar derived from their id.
func GenerateAvatarSVG(id int64, username string) []byte {
	seed := fmt.Sprintf("scruples-avatar:%d:%s", id, strings.ToLower(strings.TrimSpace(username)))
	sum := sha256.Sum256([]byte(seed))

	p := avatarPalettes[int(sum[0])%len(avatarPalettes)]
	bgStyle := sum[1] % 4
	headStyle := sum[2] % 4
	topStyle := sum[3] % 5
	eyeStyle := sum[4] % 5
	mouthStyle := sum[5] % 5
	hasCheeks := sum[6]%2 == 0

	var b bytes.Buffer
	b.WriteString(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64" width="64" height="64">`)

	// Background base and subtle geometric backdrop.
	fmt.Fprintf(&b, `<rect width="64" height="64" fill="%s"/>`, p.bg)
	switch bgStyle {
	case 0:
		fmt.Fprintf(&b, `<circle cx="52" cy="12" r="22" fill="%s" opacity="0.65"/>`, p.bgShade)
		fmt.Fprintf(&b, `<circle cx="10" cy="56" r="16" fill="%s" opacity="0.22"/>`, p.accent)
	case 1:
		fmt.Fprintf(&b, `<polygon points="0,42 64,20 64,64 0,64" fill="%s" opacity="0.65"/>`, p.bgShade)
		fmt.Fprintf(&b, `<circle cx="14" cy="14" r="10" fill="%s" opacity="0.22"/>`, p.accent)
	case 2:
		fmt.Fprintf(&b, `<circle cx="32" cy="60" r="30" fill="%s" opacity="0.7"/>`, p.bgShade)
		fmt.Fprintf(&b, `<rect x="46" y="8" width="12" height="12" rx="3" transform="rotate(20 52 14)" fill="%s" opacity="0.25"/>`, p.accent)
	default:
		fmt.Fprintf(&b, `<polygon points="0,0 38,0 18,64 0,64" fill="%s" opacity="0.6"/>`, p.bgShade)
		fmt.Fprintf(&b, `<circle cx="54" cy="52" r="14" fill="%s" opacity="0.22"/>`, p.accent)
	}

	// Ears / side accents behind the head for certain top styles.
	switch topStyle {
	case 0: // Twin round ears
		fmt.Fprintf(&b, `<circle cx="15" cy="24" r="7" fill="%s"/>`, p.accent)
		fmt.Fprintf(&b, `<circle cx="49" cy="24" r="7" fill="%s"/>`, p.accent)
	case 1: // Cat-like geometric ears
		fmt.Fprintf(&b, `<polygon points="14,29 18,12 29,21" fill="%s"/>`, p.accent)
		fmt.Fprintf(&b, `<polygon points="50,29 46,12 35,21" fill="%s"/>`, p.accent)
	case 2: // Top bun / sprout
		fmt.Fprintf(&b, `<circle cx="32" cy="13" r="8" fill="%s"/>`, p.accent)
	}

	// Head silhouette.
	switch headStyle {
	case 0: // Classic circle
		fmt.Fprintf(&b, `<circle cx="32" cy="35" r="20" fill="%s"/>`, p.face)
	case 1: // Soft squircle
		fmt.Fprintf(&b, `<rect x="12" y="16" width="40" height="38" rx="14" fill="%s"/>`, p.face)
	case 2: // Rounded capsule
		fmt.Fprintf(&b, `<rect x="14" y="15" width="36" height="40" rx="18" fill="%s"/>`, p.face)
	default: // Broad friendly oval
		fmt.Fprintf(&b, `<ellipse cx="32" cy="36" rx="21" ry="19" fill="%s"/>`, p.face)
	}

	// Foreground hair / crown accents.
	switch topStyle {
	case 3: // Swooping fringe arc
		fmt.Fprintf(&b, `<path d="M14 31 C16 15, 48 15, 50 31 C42 23, 24 23, 14 31 Z" fill="%s"/>`, p.accent)
	case 4: // Geometric crown band
		fmt.Fprintf(&b, `<rect x="17" y="15" width="30" height="7" rx="3.5" fill="%s"/>`, p.accent)
	}

	// Optional rosy cheeks.
	if hasCheeks {
		fmt.Fprintf(&b, `<circle cx="20" cy="40" r="3.2" fill="%s" opacity="0.45"/>`, p.accent)
		fmt.Fprintf(&b, `<circle cx="44" cy="40" r="3.2" fill="%s" opacity="0.45"/>`, p.accent)
	}

	// Eyes.
	switch eyeStyle {
	case 0: // Bright round eyes with catchlight
		fmt.Fprintf(&b, `<circle cx="24" cy="33" r="3.4" fill="%s"/>`, p.ink)
		fmt.Fprintf(&b, `<circle cx="40" cy="33" r="3.4" fill="%s"/>`, p.ink)
		fmt.Fprintf(&b, `<circle cx="25.1" cy="31.8" r="1.1" fill="%s"/>`, p.light)
		fmt.Fprintf(&b, `<circle cx="41.1" cy="31.8" r="1.1" fill="%s"/>`, p.light)
	case 1: // Happy curved eyes
		fmt.Fprintf(&b, `<path d="M20 34 Q24 29 28 34" fill="none" stroke="%s" stroke-width="2.8" stroke-linecap="round"/>`, p.ink)
		fmt.Fprintf(&b, `<path d="M36 34 Q40 29 44 34" fill="none" stroke="%s" stroke-width="2.8" stroke-linecap="round"/>`, p.ink)
	case 2: // Tall expressive ovals
		fmt.Fprintf(&b, `<ellipse cx="24" cy="33" rx="2.8" ry="4.2" fill="%s"/>`, p.ink)
		fmt.Fprintf(&b, `<ellipse cx="40" cy="33" rx="2.8" ry="4.2" fill="%s"/>`, p.ink)
		fmt.Fprintf(&b, `<circle cx="24.8" cy="31.4" r="1.1" fill="%s"/>`, p.light)
		fmt.Fprintf(&b, `<circle cx="40.8" cy="31.4" r="1.1" fill="%s"/>`, p.light)
	case 3: // Playful wink
		fmt.Fprintf(&b, `<circle cx="24" cy="33" r="3.4" fill="%s"/>`, p.ink)
		fmt.Fprintf(&b, `<circle cx="25.1" cy="31.8" r="1.1" fill="%s"/>`, p.light)
		fmt.Fprintf(&b, `<path d="M36 33.5 Q40 29.5 44 33.5" fill="none" stroke="%s" stroke-width="2.8" stroke-linecap="round"/>`, p.ink)
	default: // Geometric glasses
		fmt.Fprintf(&b, `<rect x="17" y="28.5" width="12" height="9" rx="3" fill="%s" fill-opacity="0.2" stroke="%s" stroke-width="2.2"/>`, p.light, p.ink)
		fmt.Fprintf(&b, `<rect x="35" y="28.5" width="12" height="9" rx="3" fill="%s" fill-opacity="0.2" stroke="%s" stroke-width="2.2"/>`, p.light, p.ink)
		fmt.Fprintf(&b, `<line x1="29" y1="33" x2="35" y2="33" stroke="%s" stroke-width="2.2"/>`, p.ink)
		fmt.Fprintf(&b, `<circle cx="23" cy="33" r="2" fill="%s"/>`, p.ink)
		fmt.Fprintf(&b, `<circle cx="41" cy="33" r="2" fill="%s"/>`, p.ink)
	}

	// Mouth.
	switch mouthStyle {
	case 0: // Warm smile
		fmt.Fprintf(&b, `<path d="M25 42 Q32 48 39 42" fill="none" stroke="%s" stroke-width="2.6" stroke-linecap="round"/>`, p.ink)
	case 1: // Big cheerful grin
		fmt.Fprintf(&b, `<path d="M24 41 Q32 50 40 41 Z" fill="%s"/>`, p.ink)
		fmt.Fprintf(&b, `<path d="M27 42.5 Q32 45 37 42.5" fill="none" stroke="%s" stroke-width="1.6" stroke-linecap="round"/>`, p.light)
	case 2: // Small curious 'o'
		fmt.Fprintf(&b, `<circle cx="32" cy="44" r="3" fill="%s"/>`, p.ink)
	case 3: // Friendly smirk
		fmt.Fprintf(&b, `<path d="M26 43 Q33 46 39 41" fill="none" stroke="%s" stroke-width="2.6" stroke-linecap="round"/>`, p.ink)
	default: // Cat-like 'w' smile
		fmt.Fprintf(&b, `<path d="M25 42.5 Q28.5 46.5 32 42.5 Q35.5 46.5 39 42.5" fill="none" stroke="%s" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"/>`, p.ink)
	}

	b.WriteString(`</svg>`)
	return b.Bytes()
}
