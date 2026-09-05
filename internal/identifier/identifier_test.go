package identifier

import "testing"

func TestEncodeIsCanonicalAndSeparatorSafe(t *testing.T) {
	first, err := Encode("plugin", "a/b", "main", "state")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Encode("plugin", "a", "b/main", "state")
	if err != nil {
		t.Fatal(err)
	}
	if first == second || first != "plugin/a%2Fb/main/state" || second != "plugin/a/b%2Fmain/state" {
		t.Fatalf("encodings collided or changed: %q, %q", first, second)
	}
}
