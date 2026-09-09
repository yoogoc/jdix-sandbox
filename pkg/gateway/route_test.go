package gateway

import "testing"

func TestParseHost(t *testing.T) {
	known := map[string]bool{
		"sbx-abc123":    true,
		"sbx-with-8000": true, // an id that itself ends in something numeric-looking
		"sbx-plain":     true,
	}
	exists := func(id string) bool { return known[id] }

	cases := []struct {
		host     string
		wantID   string
		wantPort int
		wantUser bool
		wantErr  bool
	}{
		{"sbx-abc123.sbx.example.com", "sbx-abc123", DataPlanePort, false, false},
		{"sbx-abc123-8000.sbx.example.com", "sbx-abc123", 8000, true, false},
		{"sbx-abc123-8000.sbx.example.com:443", "sbx-abc123", 8000, true, false},
		{"SBX-ABC123.SBX.EXAMPLE.COM", "sbx-abc123", DataPlanePort, false, false},
		{"sbx-abc123.sbx.example.com.", "sbx-abc123", DataPlanePort, false, false},
		// The whole label is a real sandbox, so it wins over the port reading.
		{"sbx-with-8000.sbx.example.com", "sbx-with-8000", DataPlanePort, false, false},
		{"unknown.sbx.example.com", "", 0, false, true},
		{"sbx-abc123.other.example.com", "", 0, false, true},
		{"sbx.example.com", "", 0, false, true},
		{"a.b.sbx.example.com", "", 0, false, true},
		{"sbx-abc123-99999.sbx.example.com", "", 0, false, true}, // port out of range, and no such id
	}
	for _, tc := range cases {
		got, err := ParseHost(tc.host, "sbx.example.com", exists)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseHost(%q) = %+v, want an error", tc.host, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseHost(%q): %v", tc.host, err)
			continue
		}
		if got.SandboxID != tc.wantID || got.Port != tc.wantPort || got.UserPort != tc.wantUser {
			t.Errorf("ParseHost(%q) = %+v, want id=%s port=%d user=%v",
				tc.host, got, tc.wantID, tc.wantPort, tc.wantUser)
		}
	}
}

func TestParseHostWithoutAnExistenceCheck(t *testing.T) {
	// With no resolver the port reading is taken at face value, which is what
	// the unit path wants; the server always passes a real check.
	got, err := ParseHost("sbx-abc-8000.sbx.example.com", "sbx.example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.SandboxID != "sbx-abc" || got.Port != 8000 {
		t.Fatalf("got %+v", got)
	}
}
