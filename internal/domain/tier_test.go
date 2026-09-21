package domain

import "testing"

func TestTierRoundTrip(t *testing.T) {
	for rank, text := range []string{"T0", "T1", "T2", "T3", "T4", "T5", "T6"} {
		tier, err := ParseTier(text)
		if err != nil || tier.Rank() != rank || tier.String() != text {
			t.Fatalf("round trip %q: tier=%v err=%v", text, tier, err)
		}
	}
}

func TestParseTierRejectsAnythingOutsideT0ThroughT6(t *testing.T) {
	for _, text := range []string{"", "t4", "T7", " T4", "T4 ", "4"} {
		if _, err := ParseTier(text); err == nil {
			t.Fatalf("ParseTier(%q) unexpectedly succeeded", text)
		}
	}
}
