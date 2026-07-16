package sourcekind

import "testing"

func TestSupportedIncludesXMind(t *testing.T) {
	for _, kind := range []string{"local", "git", "html", "xmind"} {
		if err := Validate(kind); err != nil {
			t.Fatalf("Validate(%q): %v", kind, err)
		}
	}
	if Supported("unknown") {
		t.Fatal("unknown source kind reported as supported")
	}
}
