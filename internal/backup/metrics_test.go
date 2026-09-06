package backup

import (
	"errors"
	"strings"
	"testing"
)

func TestMeasureDumpCountsLinesPerTableInFirstSeenOrder(t *testing.T) {
	dump := strings.Join([]string{
		"-- PostgreSQL database dump",
		"SET statement_timeout = 0;",
		`CREATE TABLE public.orders (id integer, note text);`,
		`COPY public.orders (id, note) FROM stdin;`,
		"1\tfirst",
		"2\tsecond",
		`\.`,
		`CREATE TABLE audit.trail (id integer);`,
		`COPY audit.trail (id) FROM stdin;`,
		"7",
		`\.`,
		`COPY public.orders (id, note) FROM stdin;`,
		"3\tthird",
		`\.`,
		`INSERT INTO public.orders VALUES (4, 'plain SQL is not COPY data');`,
		"",
	}, "\n")

	m, err := MeasureDump(strings.NewReader(dump))
	if err != nil {
		t.Fatal(err)
	}
	want := []TableMetric{
		{Name: "public.orders", LineCount: 3}, // repeated blocks accumulate
		{Name: "audit.trail", LineCount: 1},
	}
	if m.TableCount != len(want) || len(m.Tables) != len(want) {
		t.Fatalf("metrics = %+v, want %d tables", m, len(want))
	}
	for i, w := range want {
		if m.Tables[i] != w {
			t.Errorf("tables[%d] = %+v, want %+v", i, m.Tables[i], w)
		}
	}
}

func TestMeasureDumpHandlesQuotedTableTargets(t *testing.T) {
	dump := strings.Join([]string{
		`COPY "odd schema"."name space" (c1, "col (x)") FROM stdin;`,
		"a",
		`\.`,
		`COPY "reporting"."tbl.""q"".2026" (id) FROM stdin;`,
		"1",
		"2",
		`\.`,
		`COPY "plain" (id) FROM stdin;`,
		`\.`,
		"",
	}, "\n")

	m, err := MeasureDump(strings.NewReader(dump))
	if err != nil {
		t.Fatal(err)
	}
	want := []TableMetric{
		{Name: "odd schema.name space", LineCount: 1},
		{Name: `reporting.tbl."q".2026`, LineCount: 2},
		{Name: "plain", LineCount: 0},
	}
	if m.TableCount != len(want) {
		t.Fatalf("table count = %d, want %d (%+v)", m.TableCount, len(want), m.Tables)
	}
	for i, w := range want {
		if m.Tables[i] != w {
			t.Errorf("tables[%d] = %+v, want %+v", i, m.Tables[i], w)
		}
	}
}

func TestMeasureDumpWithoutCopyData(t *testing.T) {
	for name, dump := range map[string]string{
		"empty":            "",
		"comments only":    "-- PostgreSQL database dump\n\n",
		"insert-only dump": "-- dump\nINSERT INTO public.t VALUES (1);\nINSERT INTO public.t VALUES (2);\n",
		"copy to stdout":   "COPY (SELECT 1) TO STDOUT;\n",
	} {
		m, err := MeasureDump(strings.NewReader(dump))
		if err != nil {
			t.Fatalf("%s: err = %v", name, err)
		}
		if m.TableCount != 0 || len(m.Tables) != 0 {
			t.Errorf("%s: metrics = %+v, want zero tables", name, m)
		}
	}
}

func TestMeasureDumpUnterminatedCopyBlock(t *testing.T) {
	for name, dump := range map[string]string{
		"data then EOF":       "COPY public.t (id) FROM stdin;\n1\n2",
		"header then EOF":     "COPY public.t (id) FROM stdin;\n",
		"second block opened": "COPY a (id) FROM stdin;\n1\n\\.\nCOPY b (id) FROM stdin;\n1\n",
	} {
		if _, err := MeasureDump(strings.NewReader(dump)); err == nil {
			t.Errorf("%s: MeasureDump succeeded, want error for unterminated COPY", name)
		}
	}
}

func TestMeasureDumpAcceptsTerminatorAtEofWithoutNewline(t *testing.T) {
	m, err := MeasureDump(strings.NewReader("COPY public.t (id) FROM stdin;\n1\n2\n\\."))
	if err != nil {
		t.Fatal(err)
	}
	if m.TableCount != 1 || len(m.Tables) != 1 || m.Tables[0] != (TableMetric{Name: "public.t", LineCount: 2}) {
		t.Fatalf("metrics = %+v, want public.t with 2 lines", m)
	}
}

func TestMeasureDumpCountsWideLinesBeyondScannerLimit(t *testing.T) {
	wide := strings.Repeat("x", 128*1024) // over bufio.Scanner's 64K token limit
	m, err := MeasureDump(strings.NewReader("COPY public.wide (payload) FROM stdin;\n" + wide + "\n\\.\n"))
	if err != nil {
		t.Fatal(err)
	}
	if m.TableCount != 1 || m.Tables[0].LineCount != 1 {
		t.Fatalf("metrics = %+v, want one 1-line table", m)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestMeasureDumpReadFailure(t *testing.T) {
	if _, err := MeasureDump(failingReader{}); err == nil {
		t.Fatal("MeasureDump succeeded, want read error")
	}
}
