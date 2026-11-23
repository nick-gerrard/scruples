package web

import (
	"bytes"
	"encoding/xml"
	"io"
	"strings"
	"testing"
)

func TestGenerateAvatarSVGDeterministicAndValidXML(t *testing.T) {
	cases := []struct {
		id   int64
		name string
	}{
		{1, "nick"},
		{2, "alice"},
		{3, "bob"},
		{42, "charlie_99"},
		{999, ""},
		{7, `<script>"&'</script>`},
	}

	seen := make(map[string]int64)
	for _, tc := range cases {
		svg1 := GenerateAvatarSVG(tc.id, tc.name)
		svg2 := GenerateAvatarSVG(tc.id, tc.name)
		if !bytes.Equal(svg1, svg2) {
			t.Fatalf("GenerateAvatarSVG(%d, %q) is not deterministic", tc.id, tc.name)
		}

		// Parse full XML token stream to verify well-formedness.
		dec := xml.NewDecoder(bytes.NewReader(svg1))
		var root string
		for {
			tok, err := dec.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("GenerateAvatarSVG(%d, %q) invalid XML: %v\nsvg: %s", tc.id, tc.name, err, string(svg1))
			}
			if se, ok := tok.(xml.StartElement); ok && root == "" {
				root = se.Name.Local
			}
		}
		if root != "svg" {
			t.Fatalf("GenerateAvatarSVG(%d, %q) root element = %q, want %q", tc.id, tc.name, root, "svg")
		}

		if prev, dup := seen[string(svg1)]; dup {
			t.Errorf("GenerateAvatarSVG(%d, %q) collided with id %d", tc.id, tc.name, prev)
		}
		seen[string(svg1)] = tc.id
	}
}

func TestAvatarTemplateHelper(t *testing.T) {
	got := string(Avatar(42, `alice "the" <great>`, 32))
	checks := []string{
		`src="/avatar/42"`,
		`width="32"`,
		`height="32"`,
		`loading="lazy"`,
		`rounded-full`,
		`alt="alice &#34;the&#34; &lt;great&gt;"`,
	}
	for _, want := range checks {
		if !strings.Contains(got, want) {
			t.Errorf("Avatar() = %q, missing %q", got, want)
		}
	}
}
