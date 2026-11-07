// Package jsonfmt holds the JSON formatter the compose dialects use in place of
// bun v1.3.0's, which mis-escapes the text \u0000
// (https://github.com/uptrace/bun/issues/1472). It is internal so it can go
// when bun ships the fix.
package jsonfmt

import "bytes"

// AppendJSON appends jsonb, an encoding/json document, to b as a single-quoted
// SQL string literal, the way bun's schema.BaseDialect.AppendJSON does, with
// one correction: an escaped backslash pair (\\) is consumed as a pair, so its
// second half cannot start a \u0000 escape.
//
// bun v1.3.0 stores the \u0000 escape, which no Postgres text can hold, as the
// literal text \u0000 by doubling its backslash. Reading one byte at a time it
// mistakes the second backslash of \\u0000, encoding/json's form of the text
// \u0000 itself, for the start of that escape and emits \\\u0000, which Postgres
// rejects for jsonb and misreads for json. Quotes are doubled, a raw NUL byte
// is dropped, and any other backslash is emitted alone so the byte after it is
// escaped by the loop, as in bun.
//
// The Postgres and SQLite dialects of compose use it in place of bun's.
func AppendJSON(b, jsonb []byte) []byte {
	b = append(b, '\'')

	for i := 0; i < len(jsonb); i++ {
		c := jsonb[i]
		switch c {
		case '\'':
			b = append(b, "''"...)
		case 0:
			// Dropped, as bun does: no JSON encoder writes a raw NUL byte.
		case '\\':
			if i+1 < len(jsonb) && jsonb[i+1] == '\\' {
				b = append(b, `\\`...)
				i++
			} else if bytes.HasPrefix(jsonb[i+1:], []byte("u0000")) {
				b = append(b, `\\u0000`...)
				i += len("u0000")
			} else {
				b = append(b, '\\')
			}
		default:
			b = append(b, c)
		}
	}

	return append(b, '\'')
}
