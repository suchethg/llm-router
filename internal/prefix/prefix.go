// Package prefix computes the keys vLLM's automatic prefix caching (APC)
// would use for a prompt, so the router can predict cache hits.
//
// vLLM splits a prompt's tokens into fixed-size blocks and keys each block's
// KV cache by a hash chained over every preceding block. Block i is therefore
// reusable only if the whole prompt up to and including block i matches.
// The router has no tokenizer, so blocks here are measured in characters;
// at ~4 chars/token, 64 chars is about vLLM's default 16-token block.
package prefix

import (
	"encoding/binary"
	"hash/fnv"
	"strings"
)

// DefaultBlockChars is ~16 tokens at ~4 chars/token.
const DefaultBlockChars = 64

// Message is an OpenAI-style chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// RenderChat flattens messages into the text that would be prefilled.
//
// The format only has to be deterministic and prefix-preserving: appending a
// message must leave the rendering of earlier messages unchanged, exactly as
// real chat templates do. It doesn't need to match the model's template,
// because only equality between renderings matters.
func RenderChat(msgs []Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString("<|")
		b.WriteString(m.Role)
		b.WriteString("|>")
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
}

// BlockHashes returns one chained hash per full block of text. A trailing
// partial block is dropped, as vLLM only caches full blocks.
func BlockHashes(text string, blockChars int) []uint64 {
	if blockChars <= 0 {
		return nil
	}
	n := len(text) / blockChars
	out := make([]uint64, n)
	var prev uint64
	var buf [8]byte
	for i := 0; i < n; i++ {
		// hash(previous block's hash || this block's text)
		h := fnv.New64a()
		binary.LittleEndian.PutUint64(buf[:], prev)
		h.Write(buf[:])
		h.Write([]byte(text[i*blockChars : (i+1)*blockChars]))
		prev = h.Sum64()
		out[i] = prev
	}
	return out
}
