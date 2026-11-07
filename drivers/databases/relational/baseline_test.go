package relational

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

const framedDump = `--
-- PostgreSQL database dump
--

\restrict K3y
-- Dumped from database version 16.14

SET statement_timeout = 0;
CREATE FUNCTION public.f() RETURNS text LANGUAGE sql AS $$
SELECT '
\restrict K3y
\unrestrict K3y
'
$$;

\unrestrict K3y

`

func TestStripRestrictFramingDropsOnlyTheFrame(t *testing.T) {
	got := stripRestrictFraming(framedDump)

	// The frame is gone; the body lines of the same shape stay, as does the rest
	assert.NotContains(t, got, "\n\\restrict K3y\n-- Dumped")
	assert.NotContains(t, got, "$$;\n\n\\unrestrict K3y\n")
	assert.Contains(t, got, "SELECT '\n\\restrict K3y\n\\unrestrict K3y\n'\n")
	assert.Equal(t, strings.Count(framedDump, "\n")-2, strings.Count(got, "\n"))
	assert.True(t, strings.HasPrefix(got, "--\n-- PostgreSQL database dump\n--\n\n-- Dumped from"))
	assert.True(t, strings.HasSuffix(got, "$$;\n\n\n"))
}

func TestStripRestrictFramingLeavesAnUnframedDumpAlone(t *testing.T) {
	dump := `-- No frame in this dump
SET statement_timeout = 0;
CREATE FUNCTION public.f() RETURNS text LANGUAGE sql AS $$
SELECT '
\restrict abc
\unrestrict abc
'
$$;
`

	// The body holds both frame lines exactly, so a match anywhere would strip them
	assert.Equal(t, dump, stripRestrictFraming(dump))
}

func TestStripRestrictFramingNeedsTheOpeningFirst(t *testing.T) {
	// The exact closing line is last, but the opening sits inside a body, after SQL
	dump := `SET statement_timeout = 0;
CREATE FUNCTION public.f() RETURNS text LANGUAGE sql AS $$
SELECT '
\restrict abc
'
$$;
\unrestrict abc
`

	assert.Equal(t, dump, stripRestrictFraming(dump))
}

func TestStripRestrictFramingNeedsTheClosingLast(t *testing.T) {
	// The opening is first and a body holds the exact closing line, but SQL follows it
	dump := `\restrict abc
CREATE FUNCTION public.f() RETURNS text LANGUAGE sql AS $$
SELECT '
\unrestrict abc
'
$$;
SELECT 1;
`

	assert.Equal(t, dump, stripRestrictFraming(dump))
}

func TestStripRestrictFramingLeavesAnUnclosedFrameAlone(t *testing.T) {
	noClose := "\\restrict abc\nSELECT 1;\n"
	otherKey := "\\restrict abc\nSELECT 1;\n\\unrestrict other\n"

	assert.Equal(t, noClose, stripRestrictFraming(noClose))
	assert.Equal(t, otherKey, stripRestrictFraming(otherKey))
}

func TestStripRestrictFramingToleratesCRLF(t *testing.T) {
	dump := strings.ReplaceAll(framedDump, "\n", "\r\n")

	got := stripRestrictFraming(dump)

	assert.NotContains(t, got, "\r\n\\restrict K3y\r\n-- Dumped")
	assert.NotContains(t, got, "$$;\r\n\r\n\\unrestrict K3y\r\n")
	assert.Contains(t, got, "SELECT '\r\n\\restrict K3y\r\n\\unrestrict K3y\r\n'\r\n")
	assert.Equal(t, strings.Count(dump, "\r\n")-2, strings.Count(got, "\r\n"))
}

func TestStripRestrictFramingHandlesAOneLineDump(t *testing.T) {
	assert.Equal(t, "\\restrict abc", stripRestrictFraming("\\restrict abc"))
	assert.Equal(t, "", stripRestrictFraming(""))
}
