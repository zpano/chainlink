package realcap_test

import stdstrings "strings"

// The simulated-chain test is intentionally isolated from production code.
// Keep the helper in a separate test file so the large integration harness
// remains focused on the EVM service adapter and assertions.
var strings = struct {
	Repeat func(string, int) string
}{
	Repeat: stdstrings.Repeat,
}
