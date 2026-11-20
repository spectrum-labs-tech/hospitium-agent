// SPDX-License-Identifier: Apache-2.0

package tv

import "testing"

// Outputs as a Fire TV (Fire OS 8.1.8.5) printed them.
func TestParseWakefulness(t *testing.T) {
	t.Parallel()
	for out, want := range map[string]bool{
		"POWER MANAGER (dumpsys power)\n  mWakefulness=Awake\n  mWakefulnessChanging=false": true,
		"  mWakefulness=Asleep\n":   false,
		"  mWakefulness=Dreaming\n": false,
		"  mWakefulness=Dozing\n":   false,
	} {
		got, err := parseWakefulness(out)
		if err != nil || got != want {
			t.Errorf("%q: %v %v, want %v", out, got, err, want)
		}
	}
	if _, err := parseWakefulness("nothing useful"); err == nil {
		t.Error("output without mWakefulness should be an error, not 'asleep'")
	}
}

func TestParseFocus(t *testing.T) {
	t.Parallel()
	for out, want := range map[string]string{
		"  mCurrentFocus=Window{81bca78 u0 com.amazon.tv.launcher/com.amazon.tv.launcher.ui.HomeActivity_vNext}":                    "com.amazon.tv.launcher",
		"  mCurrentFocus=Window{60aac6c u0 tv.hospitium.launcher/tv.hospitium.launcher.MainActivity}": "tv.hospitium.launcher",
		"  mCurrentFocus=null": "",
	} {
		got, err := parseFocus(out)
		if err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", out, got, err, want)
		}
	}
	if _, err := parseFocus(""); err == nil {
		t.Error("output without mCurrentFocus should be an error")
	}
}
