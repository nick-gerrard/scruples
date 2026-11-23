package web

import (
	"bytes"
	"encoding/xml"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"scruples/internal/db"
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
		`src="/avatar/42?v=gen"`,
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

func TestAppAvatarHelperWithDB(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "avatar-helper.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	res, err := database.Exec(`INSERT INTO users (username, password_hash) VALUES ('alice', 'hash')`)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	uid, _ := res.LastInsertId()
	uidStr := strconv.FormatInt(uid, 10)
	app := NewApp(database)

	before := string(app.Avatar(uid, "alice", 40))
	if !strings.Contains(before, `src="/avatar/`+uidStr+`?v=gen"`) {
		t.Fatalf("app.Avatar before upload = %q, want ?v=gen", before)
	}

	const avatarPath = "avatars/1-deadbeefcafebabe.jpg"
	if _, err := database.Exec(`UPDATE users SET avatar_path = ? WHERE id = ?`, avatarPath, uid); err != nil {
		t.Fatalf("update avatar_path: %v", err)
	}
	wantV := AvatarVersion(avatarPath)
	after := string(app.Avatar(uid, "alice", 40))
	if !strings.Contains(after, `src="/avatar/`+uidStr+`?v=`+wantV+`"`) {
		t.Fatalf("app.Avatar after upload = %q, want ?v=%s", after, wantV)
	}
}
