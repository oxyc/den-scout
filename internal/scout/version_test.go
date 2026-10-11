package scout

import "testing"

func TestVersionFrom(t *testing.T) {
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == "DEN_VERSION" {
				return v
			}
			return ""
		}
	}
	if got := versionFrom(env("1.2.3")); got != "1.2.3" {
		t.Fatalf("DEN_VERSION set: got %q", got)
	}
	if got := versionFrom(env("")); got != devVersion {
		t.Fatalf("DEN_VERSION unset: got %q, want %q", got, devVersion)
	}
}
