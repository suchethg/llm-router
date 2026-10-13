package prefix

import (
	"strings"
	"testing"
)

// blocks builds text from one 4-char block per letter, e.g. "ab" -> "aaaabbbb".
func blocks(letters string) string {
	var b strings.Builder
	for _, c := range letters {
		b.WriteString(strings.Repeat(string(c), 4))
	}
	return b.String()
}

func TestSharedPrefixHashesEqual(t *testing.T) {
	a := BlockHashes(blocks("abcx"), 4)
	b := BlockHashes(blocks("abcy"), 4)
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			t.Fatalf("block %d of a shared prefix differs", i)
		}
	}
	if a[3] == b[3] {
		t.Fatal("divergent last block hashed equal")
	}
}

// Changing one block must change it and every later block, even when the
// later text is identical; that is what limits reuse to exact prefixes.
func TestDivergenceInvalidatesLaterBlocks(t *testing.T) {
	a := BlockHashes(blocks("abcd"), 4)
	b := BlockHashes(blocks("aXcd"), 4)
	if a[0] != b[0] {
		t.Fatal("block before the change should match")
	}
	for i := 1; i < 4; i++ {
		if a[i] == b[i] {
			t.Fatalf("block %d matched after an earlier block changed", i)
		}
	}
}

func TestSameBlockDifferentPrefixDiffers(t *testing.T) {
	if BlockHashes(blocks("ab"), 4)[1] == BlockHashes(blocks("zb"), 4)[1] {
		t.Fatal(`"b" after "a" and "b" after "z" must hash differently (chaining)`)
	}
}

func TestPartialBlockDropped(t *testing.T) {
	for _, tc := range []struct{ chars, want int }{{0, 0}, {63, 0}, {64, 1}, {127, 1}, {128, 2}} {
		if got := len(BlockHashes(strings.Repeat("x", tc.chars), 64)); got != tc.want {
			t.Errorf("%d chars: %d blocks, want %d", tc.chars, got, tc.want)
		}
	}
	if BlockHashes("abc", 0) != nil {
		t.Error("non-positive block size should yield no blocks")
	}
}

func TestRenderChatPrefixPreserving(t *testing.T) {
	turn1 := []Message{{"system", "be helpful"}, {"user", "hi"}}
	turn2 := append(append([]Message{}, turn1...), Message{"assistant", "hello"}, Message{"user", "more"})
	r1, r2 := RenderChat(turn1), RenderChat(turn2)
	if !strings.HasPrefix(r2, r1) {
		t.Fatal("appending messages must preserve the earlier rendering")
	}
	// So every full block of turn 1 is reusable by turn 2.
	h1, h2 := BlockHashes(r1, 8), BlockHashes(r2, 8)
	for i := range h1 {
		if h1[i] != h2[i] {
			t.Fatalf("turn 1 block %d not reusable by turn 2", i)
		}
	}
}

func TestRenderChatRoleMatters(t *testing.T) {
	if RenderChat([]Message{{"user", "x"}}) == RenderChat([]Message{{"assistant", "x"}}) {
		t.Fatal("same content under different roles must render differently")
	}
}
