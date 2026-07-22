package effects

import "testing"

func TestGet_WarpStabilizerRegistered(t *testing.T) {
	e, err := Get("warp-stabilizer")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if e.Name() != "warp-stabilizer" {
		t.Errorf("got name %q, want %q", e.Name(), "warp-stabilizer")
	}
	if e.FilenameSlug() != "stabilized" {
		t.Errorf("got slug %q, want %q", e.FilenameSlug(), "stabilized")
	}
}

func TestGet_UnknownEffect(t *testing.T) {
	_, err := Get("does-not-exist")
	if err == nil {
		t.Fatal("expected an error for an unknown effect, got nil")
	}
}

func TestValidateUnitRange(t *testing.T) {
	cases := []struct {
		strength float64
		wantErr  bool
	}{
		{-0.1, true},
		{0.0, false},
		{0.5, false},
		{1.0, false},
		{1.1, true},
	}
	for _, c := range cases {
		err := ValidateUnitRange(c.strength)
		if (err != nil) != c.wantErr {
			t.Errorf("ValidateUnitRange(%v): got err=%v, wantErr=%v", c.strength, err, c.wantErr)
		}
	}
}
