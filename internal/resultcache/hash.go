package resultcache

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/zeebo/xxh3"
)

// hash128Hex returns the canonical lowercase 32-char hex rendering of an
// xxh3-128 digest used for both cache keys and envelope checksums.
func hash128Hex(data string) string {
	sum := xxh3.HashString128(data)
	return fmt.Sprintf("%016x%016x", sum.Hi, sum.Lo)
}

// frameHash hashes length-prefixed parts so that fields can never bleed into
// each other (e.g. ["ab","c"] and ["a","bc"] produce different digests).
// Every caller passes fixed-shape, semantically distinct parts.
func frameHash(parts ...string) string {
	var builder strings.Builder
	for _, part := range parts {
		builder.WriteString(strconv.Itoa(len(part)))
		builder.WriteByte(':')
		builder.WriteString(part)
		builder.WriteByte('\n')
	}
	return hash128Hex(builder.String())
}

// ContentHash is the xxh3-128 hex digest of one file's exact complete text,
// including its byte order mark. Callers MUST feed the same representation the
// linter parsed (TargetProjection.ReadText provides it).
func ContentHash(text string) string {
	return hash128Hex(text)
}
