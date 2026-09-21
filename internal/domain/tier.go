package domain

import "fmt"

// Tier is a provider-neutral capability floor. Higher values indicate a
// stronger model capability requirement.
type Tier uint8

const (
	T0 Tier = iota
	T1
	T2
	T3
	T4
	T5
	T6
)

// ParseTier parses only the seven supported capability tiers.
func ParseTier(text string) (Tier, error) {
	switch text {
	case "T0":
		return T0, nil
	case "T1":
		return T1, nil
	case "T2":
		return T2, nil
	case "T3":
		return T3, nil
	case "T4":
		return T4, nil
	case "T5":
		return T5, nil
	case "T6":
		return T6, nil
	default:
		return T0, fmt.Errorf("unknown tier %q", text)
	}
}

// Rank returns the ordered capability rank of the tier.
func (t Tier) Rank() int {
	return int(t)
}

// Valid reports whether t is one of the configured capability tiers.
func (t Tier) Valid() bool {
	return t <= T6
}

func (t Tier) String() string {
	if t.Valid() {
		return fmt.Sprintf("T%d", t)
	}
	return fmt.Sprintf("Tier(%d)", t)
}
