package shoalql

import "testing"

func TestParse_ExplainFormats(t *testing.T) {
	for _, tc := range []struct {
		sql    string
		format ExplainFormat
	}{
		{"EXPLAIN SELECT id FROM events", ExplainText},
		{"explain format text select id from events", ExplainText},
		{"EXPLAIN FORMAT JSON SELECT id FROM events", ExplainJSON},
	} {
		st, err := Parse(tc.sql)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.sql, err)
		}
		if !st.Explain || st.ExplainFormat != tc.format {
			t.Errorf("Parse(%q): explain=%v format=%q", tc.sql, st.Explain, st.ExplainFormat)
		}
	}
	if _, err := Parse("EXPLAIN FORMAT YAML SELECT id FROM events"); err == nil {
		t.Fatal("expected unsupported EXPLAIN format error")
	}
}

func TestParse_SelectStarBasic(t *testing.T) {
	st, err := Parse("SELECT * FROM events")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Star || st.Table != "events" {
		t.Fatalf("got %+v", st)
	}
}

func TestParse_ColumnsAndAliases(t *testing.T) {
	st, err := Parse("SELECT id, content AS body FROM events LIMIT 5")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Columns) != 2 {
		t.Fatalf("cols = %d", len(st.Columns))
	}
	if st.Columns[0].Column != "id" {
		t.Errorf("col0 = %+v", st.Columns[0])
	}
	if st.Columns[1].Column != "content" || st.Columns[1].Alias != "body" {
		t.Errorf("col1 = %+v", st.Columns[1])
	}
	if st.Limit == nil || *st.Limit != 5 {
		t.Errorf("limit = %v", st.Limit)
	}
}

func TestParse_WhereRangeAndLike(t *testing.T) {
	st, err := Parse("SELECT id FROM events WHERE id >= 'a' AND id < 'b' AND content LIKE 'foo%'")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Where) != 3 {
		t.Fatalf("preds = %d", len(st.Where))
	}
	if st.Where[0].Op != OpGE || st.Where[0].Value.Str != "a" {
		t.Errorf("p0 = %+v", st.Where[0])
	}
	if st.Where[2].Op != OpLike || st.Where[2].Value.Str != "foo%" {
		t.Errorf("p2 = %+v", st.Where[2])
	}
}

func TestParse_Match(t *testing.T) {
	st, err := Parse("SELECT id FROM events WHERE MATCH(content, 'retry timeout')")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Where) != 1 || st.Where[0].Kind != PredMatch {
		t.Fatalf("where = %+v", st.Where)
	}
	if st.Where[0].Column != "content" || st.Where[0].MatchTerms != "retry timeout" {
		t.Errorf("match = %+v", st.Where[0])
	}
}

func TestParse_OrderByVectorForms(t *testing.T) {
	cases := []struct {
		sql  string
		kind VecKind
	}{
		{"SELECT id FROM events ORDER BY embedding <-> [0.1, -0.2, 0.3] LIMIT 3", VecLiteral},
		{"SELECT id FROM events ORDER BY embedding <-> :q LIMIT 3", VecParam},
		{"SELECT id FROM events ORDER BY embedding <-> 'find me' LIMIT 3", VecText},
	}
	for _, c := range cases {
		st, err := Parse(c.sql)
		if err != nil {
			t.Fatalf("%s: %v", c.sql, err)
		}
		if st.Order == nil || st.Order.Column != "embedding" {
			t.Fatalf("%s: order = %+v", c.sql, st.Order)
		}
		if st.Order.Target.Kind != c.kind {
			t.Errorf("%s: kind = %v want %v", c.sql, st.Order.Target.Kind, c.kind)
		}
	}
}

func TestParse_VectorLiteralValues(t *testing.T) {
	st, err := Parse("SELECT id FROM events ORDER BY vec <-> [1, 2.5, -3]")
	if err != nil {
		t.Fatal(err)
	}
	got := st.Order.Target.Literal
	want := []float32{1, 2.5, -3}
	if len(got) != len(want) {
		t.Fatalf("len = %d", len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("v[%d] = %v want %v", i, got[i], want[i])
		}
	}
}

func TestParse_CountGroupBy(t *testing.T) {
	st, err := Parse("SELECT cf, count(*) AS n FROM events GROUP BY cf")
	if err != nil {
		t.Fatal(err)
	}
	if st.GroupBy != "cf" {
		t.Fatalf("groupby = %q", st.GroupBy)
	}
	if !st.Columns[1].CountStar || st.Columns[1].Alias != "n" {
		t.Errorf("col1 = %+v", st.Columns[1])
	}
}

func TestParse_Expand(t *testing.T) {
	st, err := Parse("SELECT expand(id, 'semantic') AS nbr FROM events WHERE id = 'evt:1'")
	if err != nil {
		t.Fatal(err)
	}
	it := st.Columns[0]
	if !it.isExpand() || it.ExpandCol != "id" || it.ExpandEdge != "semantic" || it.Alias != "nbr" {
		t.Fatalf("expand item = %+v", it)
	}
}

func TestParse_AsOf(t *testing.T) {
	st, err := Parse("SELECT * FROM events AS OF 1700000000000 WHERE id = 'evt:1'")
	if err != nil {
		t.Fatal(err)
	}
	if st.AsOf == nil || *st.AsOf != 1700000000000 {
		t.Fatalf("asof = %v", st.AsOf)
	}
}

func TestParse_Errors(t *testing.T) {
	bad := []string{
		"SELECT",
		"SELECT * events",
		"SELECT * FROM",
		"SELECT count(id) FROM events",
		"SELECT * FROM events ORDER BY embedding = 'x'",
		"SELECT * FROM events ORDER BY embedding <-> []",
		"SELECT * FROM events LIMIT abc",
		"DELETE FROM events",
		"SELECT * FROM events WHERE",
	}
	for _, s := range bad {
		if _, err := Parse(s); err == nil {
			t.Errorf("expected error for %q", s)
		}
	}
}
