package nodejs

import "testing"

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in        string
		major     int
		minor     int
		shouldMin bool
		wantErr   bool
	}{
		{"v22.5.0", 22, 5, true, false},
		{"22.14.0", 22, 14, true, false},
		{"v24.1.0", 24, 1, true, false},
		{"v22.4.1", 22, 4, false, false},
		{"v21.7.3", 21, 7, false, false},
		{"v18.19.0", 18, 19, false, false},
		{"", 0, 0, false, true},
		{"node", 0, 0, false, true},
	}
	for _, c := range cases {
		major, minor, err := parseVersion(c.in)
		if (err != nil) != c.wantErr {
			t.Fatalf("parseVersion(%q) err=%v, wantErr=%v", c.in, err, c.wantErr)
		}
		if err != nil {
			continue
		}
		if major != c.major || minor != c.minor {
			t.Fatalf("parseVersion(%q) = %d.%d, want %d.%d", c.in, major, minor, c.major, c.minor)
		}
		if got := versionAtLeast(major, minor); got != c.shouldMin {
			t.Fatalf("versionAtLeast(%d,%d) = %v, want %v", major, minor, got, c.shouldMin)
		}
	}
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"china": "china", "CN": "china", "cn": "china",
		"global": "global", "Global": "global", "intl": "global",
	} {
		if got := Normalize(in); got != want {
			t.Fatalf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}
