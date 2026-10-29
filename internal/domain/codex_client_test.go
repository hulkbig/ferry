package domain

import "testing"

func TestCodexClientVersionValidation(t *testing.T) {
	for _, version := range []string{"", "0.153.0", "1.2.3-beta.1"} {
		if err := ValidateCodexClientVersion(version); err != nil {
			t.Fatal(err)
		}
	}
	for _, version := range []string{"latest", "0.153", " 0.153.0", "0.153.0\r\nInjected: value", "00.153.0"} {
		if ValidateCodexClientVersion(version) == nil {
			t.Fatalf("accepted invalid version %q", version)
		}
	}
}
