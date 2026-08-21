package chunk

// gear is the 256-entry table of the Gear rolling hash used by FastCDC.
//
// It is derived deterministically from a fixed seed with SplitMix64 rather
// than embedded as 256 literals: the derivation is short enough to reproduce
// in another language, and TestGearTableIsPinned fails if it ever changes,
// because a different table would move every chunk boundary and therefore
// every pointer produced by cdc-fastcdc/1.
var gear = buildGear(0x1FE7_F1A9_C0DE_5EED)

func buildGear(seed uint64) [256]uint64 {
	var t [256]uint64
	s := seed
	for i := range t {
		s += 0x9E3779B97F4A7C15
		z := s
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		t[i] = z ^ (z >> 31)
	}
	return t
}
