package version

import "testing"

func TestResolve(t *testing.T) {
	cases := []struct {
		name    string
		stamped string
		module  string
		want    string
	}{
		{"stamped release build", "v0.8.5", "(devel)", "v0.8.5"},
		{"stamp wins over module version", "v0.8.5", "v0.9.0", "v0.8.5"},
		{"module version from go install", Default, "v0.8.5", "v0.8.5"},
		{"plain local build", Default, "(devel)", Default},
		{"empty stamp and module", "", "", Default},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolve(tc.stamped, tc.module); got != tc.want {
				t.Fatalf("resolve(%q, %q) = %q, want %q", tc.stamped, tc.module, got, tc.want)
			}
		})
	}
}

func TestValueIsReportable(t *testing.T) {
	if Value == "" {
		t.Fatal("Value must never be empty")
	}
	if Value == "(devel)" {
		t.Fatalf("Value = %q leaks the Go module placeholder", Value)
	}
}
