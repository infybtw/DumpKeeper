package backup

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
)

// TableMetric is the physical COPY data-line count for one table in a
// plain-text dump. LineCount is the literal line unit: because COPY encodes
// embedded newlines, each data line is one exported row.
type TableMetric struct {
	Name      string
	LineCount int64
}

// DumpMetrics summarizes the COPY blocks of a plain-text pg_dump file.
// Tables preserves first-seen order; repeated COPY blocks for one table
// accumulate into that table's single entry.
type DumpMetrics struct {
	TableCount int
	Tables     []TableMetric
}

// MeasureDump streams a plain-text SQL dump and counts, per table, the
// physical data lines of every `COPY <table> (<columns>) FROM stdin;` block
// (the terminating `\.` line is not data). A dump without COPY blocks —
// schema-only dumps, custom formats, plain SQL — measures as zero tables.
// Truncated or unreadable input returns an error instead of a wrong count.
// The reader is consumed to EOF and never closed.
func MeasureDump(r io.Reader) (DumpMetrics, error) {
	// bufio.Reader, not Scanner: a wide COPY row must not hit a token limit.
	reader := bufio.NewReader(r)
	var metrics DumpMetrics
	index := make(map[string]int) // table name -> position in Tables
	inCopy := false
	var current *TableMetric
	for {
		line, rerr := reader.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		if inCopy {
			if trimmed == `\.` {
				inCopy = false
				current = nil
			} else if len(line) > 0 || rerr == nil {
				// A data line may look like anything — including "COPY " or
				// "GRANT " — so only the terminator is special.
				current.LineCount++
			}
		} else if name, ok := copyTable(trimmed); ok {
			if i, seen := index[name]; seen {
				current = &metrics.Tables[i]
			} else {
				index[name] = len(metrics.Tables)
				metrics.Tables = append(metrics.Tables, TableMetric{Name: name})
				current = &metrics.Tables[len(metrics.Tables)-1]
				metrics.TableCount = len(metrics.Tables)
			}
			inCopy = true
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				return DumpMetrics{}, fmt.Errorf("read dump: %w", rerr)
			}
			break
		}
	}
	if inCopy {
		return DumpMetrics{}, fmt.Errorf("dump: COPY block for table %q is not terminated", current.Name)
	}
	return metrics, nil
}

// copyTable extracts the display name from a plain-text COPY header, e.g.
// `COPY public."odd name" (id) FROM stdin;` -> `public.odd name`. Lines that
// are not `COPY <table> (<columns>) FROM stdin;` headers report ok=false.
func copyTable(line string) (string, bool) {
	rest, ok := strings.CutPrefix(line, "COPY ")
	if !ok {
		return "", false
	}
	rest, ok = strings.CutSuffix(rest, " FROM stdin;")
	if !ok {
		return "", false
	}
	// rest is `<table target> (<column list>)`; the target ends at the first
	// " (" that sits outside a quoted identifier.
	end := tableTargetEnd(rest)
	if end <= 0 {
		return "", false
	}
	return unquoteTable(rest[:end]), true
}

// tableTargetEnd returns the index of the space that precedes the column
// list's opening parenthesis, or -1 when no such boundary exists. Quoted
// identifiers may contain spaces, dots, parentheses, and doubled quotes.
func tableTargetEnd(rest string) int {
	quoted := false
	for i := range len(rest) {
		switch rest[i] {
		case '"':
			quoted = !quoted
		case ' ':
			if !quoted && i+1 < len(rest) && rest[i+1] == '(' {
				return i
			}
		}
	}
	return -1
}

// unquoteTable renders a COPY table target for display: SQL quoting is
// removed, doubled quotes unescape to one, and dots inside quoted
// identifiers are kept as part of the name rather than separators.
func unquoteTable(target string) string {
	var b strings.Builder
	b.Grow(len(target))
	quoted := false
	for i := 0; i < len(target); i++ {
		switch c := target[i]; {
		case c == '"':
			if quoted && i+1 < len(target) && target[i+1] == '"' {
				b.WriteByte('"')
				i++
			} else {
				quoted = !quoted
			}
		case c == '.' && !quoted:
			b.WriteByte('.')
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
