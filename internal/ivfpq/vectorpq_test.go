package ivfpq

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"math"
	"strconv"
	"strings"
	"testing"
)

// makeCodebook builds a deterministic codebook of size [m][ks][dsub]
// with values that vary on every axis so dot products and code lookups
// give distinct results — useful for round-trip + scoring sanity tests.
func makeCodebook(m, ks, dsub int) [][][]float32 {
	cb := make([][][]float32, m)
	for s := 0; s < m; s++ {
		cb[s] = make([][]float32, ks)
		for c := 0; c < ks; c++ {
			row := make([]float32, dsub)
			for d := 0; d < dsub; d++ {
				row[d] = float32(s*1000+c*10+d) / 1000.0
			}
			cb[s][c] = row
		}
	}
	return cb
}

func TestVectorPQ_RoundtripSmall(t *testing.T) {
	cb := makeCodebook(4, 8, 4)
	pq := &VectorPQ{
		codebook:        cb,
		m:               4,
		ks:              8,
		dim:             16,
		dsub:            4,
		codebookVersion: 7,
	}
	var buf bytes.Buffer
	if _, err := pq.WriteTo(&buf); err != nil {
		t.Fatal(err)
	}
	parsed, err := FromBytes(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if parsed.M() != 4 || parsed.Ks() != 8 || parsed.Dim() != 16 || parsed.dsub != 4 {
		t.Errorf("dims mismatch: M=%d Ks=%d dim=%d dsub=%d", parsed.M(), parsed.Ks(), parsed.Dim(), parsed.dsub)
	}
	if parsed.CodebookVersion() != 7 {
		t.Errorf("codebookVersion = %d; want 7", parsed.CodebookVersion())
	}
	// Spot-check a few cells of the codebook.
	for _, p := range []struct{ s, c, d int }{{0, 0, 0}, {2, 5, 1}, {3, 7, 3}} {
		want := cb[p.s][p.c][p.d]
		got := parsed.codebook[p.s][p.c][p.d]
		if want != got {
			t.Errorf("codebook[%d][%d][%d] = %f; want %f", p.s, p.c, p.d, got, want)
		}
	}
}

func TestVectorPQ_FromBytes_BadFormat(t *testing.T) {
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, int32(99))    // bogus FORMAT_VERSION
	binary.Write(&buf, binary.BigEndian, int32(1))     // codebookVersion
	binary.Write(&buf, binary.BigEndian, int32(1))     // m
	binary.Write(&buf, binary.BigEndian, int32(1))     // ks
	binary.Write(&buf, binary.BigEndian, int32(1))     // dim
	binary.Write(&buf, binary.BigEndian, float32(0.0)) // codebook[0][0][0]
	if _, err := FromBytes(buf.Bytes()); err == nil ||
		!strings.Contains(err.Error(), "unsupported VectorPQ format") {
		t.Errorf("expected unsupported-format error; got %v", err)
	}
}

func TestVectorPQ_FromBytes_DimNotDivisibleByM(t *testing.T) {
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, FormatVersion)
	binary.Write(&buf, binary.BigEndian, int32(1))  // codebookVersion
	binary.Write(&buf, binary.BigEndian, int32(3))  // m
	binary.Write(&buf, binary.BigEndian, int32(2))  // ks
	binary.Write(&buf, binary.BigEndian, int32(10)) // dim — not divisible by 3
	if _, err := FromBytes(buf.Bytes()); err == nil ||
		!strings.Contains(err.Error(), "not divisible") {
		t.Errorf("expected divisibility error; got %v", err)
	}
}

func TestVectorPQ_InnerProductTable(t *testing.T) {
	cb := makeCodebook(2, 4, 3)
	pq := &VectorPQ{codebook: cb, m: 2, ks: 4, dim: 6, dsub: 3, codebookVersion: 0}
	query := []float32{1, 2, 3, 4, 5, 6}
	tbl, err := pq.InnerProductTable(query)
	if err != nil {
		t.Fatal(err)
	}
	if len(tbl) != 2 || len(tbl[0]) != 4 {
		t.Fatalf("table dims: %d × %d", len(tbl), len(tbl[0]))
	}
	// Verify subspace 0 entry 0: dot(query[0:3], cb[0][0])
	want := float32(0)
	for i := 0; i < 3; i++ {
		want += query[i] * cb[0][0][i]
	}
	if tbl[0][0] != want {
		t.Errorf("tbl[0][0] = %f; want %f", tbl[0][0], want)
	}
}

func TestVectorPQ_DotMatchesNaiveDotOfDecoded(t *testing.T) {
	// Synthesize a small codebook + a code that picks a known centroid in
	// each subspace. The PQ Dot() must equal the dot product between the
	// query and the literal concatenation of those centroids.
	cb := makeCodebook(3, 4, 5)
	pq := &VectorPQ{codebook: cb, m: 3, ks: 4, dim: 15, dsub: 5}
	query := make([]float32, 15)
	for i := range query {
		query[i] = float32(i+1) / 7.0
	}
	tbl, _ := pq.InnerProductTable(query)
	code := []byte{2, 0, 3} // one byte per subspace

	want := float32(0)
	for s := 0; s < 3; s++ {
		entry := cb[s][int(code[s])]
		for d := 0; d < 5; d++ {
			want += query[s*5+d] * entry[d]
		}
	}
	got := pq.Dot(code, tbl)
	const eps = 1e-5
	if math.Abs(float64(got-want)) > eps {
		t.Errorf("Dot = %f; want %f (Δ=%g)", got, want, math.Abs(float64(got-want)))
	}
}

func TestIterator_TopKFromOptions(t *testing.T) {
	cb := makeCodebook(2, 4, 2)
	pq := &VectorPQ{codebook: cb, m: 2, ks: 4, dim: 4, dsub: 2}
	var pqBuf bytes.Buffer
	if _, err := pq.WriteTo(&pqBuf); err != nil {
		t.Fatal(err)
	}

	query := []float32{1, 1, 1, 1}
	queryBuf := make([]byte, len(query)*4)
	for i, f := range query {
		binary.BigEndian.PutUint32(queryBuf[i*4:], math.Float32bits(f))
	}

	it, err := NewFromOptions(map[string]string{
		OptPQCodebook: base64.StdEncoding.EncodeToString(pqBuf.Bytes()),
		OptQuery:      base64.StdEncoding.EncodeToString(queryBuf),
		OptTopK:       "2",
		OptThreshold:  strconv.FormatFloat(-1e30, 'g', -1, 32),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Offer 4 cells with code → enumerate scores via Dot(); top-2 by
	// score should win regardless of insertion order.
	tbl, _ := pq.InnerProductTable(query)
	codes := [][]byte{{0, 0}, {1, 1}, {2, 2}, {3, 3}}
	rows := []string{"r0", "r1", "r2", "r3"}
	scores := make([]float32, len(codes))
	for i, c := range codes {
		scores[i] = pq.Dot(c, tbl)
		it.Offer([]byte(rows[i]), nil, nil, nil, int64(i), c)
	}

	out := it.Drain()
	if len(out) != 2 {
		t.Fatalf("len(out) = %d; want 2", len(out))
	}
	// Sort scores descending; check the top-2 rows match.
	type rs struct {
		row   string
		score float32
	}
	all := make([]rs, len(rows))
	for i := range rows {
		all[i] = rs{rows[i], scores[i]}
	}
	// Bubble — N=4.
	for i := 0; i < len(all); i++ {
		for j := i + 1; j < len(all); j++ {
			if all[j].score > all[i].score {
				all[i], all[j] = all[j], all[i]
			}
		}
	}
	wantTop := map[string]bool{all[0].row: true, all[1].row: true}
	for _, c := range out {
		if !wantTop[string(c.Row)] {
			t.Errorf("unexpected row %q in top-2 (top-2 = %v)", string(c.Row), wantTop)
		}
	}
	// Decoded score in Value matches the actual cell's score.
	gotScore := math.Float32frombits(binary.BigEndian.Uint32(out[0].Value))
	if math.Abs(float64(gotScore-all[0].score)) > 1e-5 {
		t.Errorf("emitted score = %f; want %f", gotScore, all[0].score)
	}
}

func TestIterator_DropsWrongLengthCodes(t *testing.T) {
	pq := &VectorPQ{codebook: makeCodebook(2, 4, 2), m: 2, ks: 4, dim: 4, dsub: 2}
	var pqBuf bytes.Buffer
	pq.WriteTo(&pqBuf)
	queryBuf := make([]byte, 4*4)
	for i := 0; i < 4; i++ {
		binary.BigEndian.PutUint32(queryBuf[i*4:], math.Float32bits(1.0))
	}
	it, err := NewFromOptions(map[string]string{
		OptPQCodebook: base64.StdEncoding.EncodeToString(pqBuf.Bytes()),
		OptQuery:      base64.StdEncoding.EncodeToString(queryBuf),
		OptThreshold:  "-1e30",
	})
	if err != nil {
		t.Fatal(err)
	}
	it.Offer([]byte("r0"), nil, nil, nil, 1, []byte{0, 0})       // valid → counted
	it.Offer([]byte("r1"), nil, nil, nil, 2, []byte{0, 0, 0})    // wrong length → dropped
	it.Offer([]byte("r2"), nil, nil, nil, 3, []byte{})           // empty → dropped
	it.Offer([]byte("r3"), nil, nil, nil, 4, []byte{1, 1, 1, 1}) // wrong length → dropped
	out := it.Drain()
	if len(out) != 1 || string(out[0].Row) != "r0" {
		t.Errorf("expected only r0; got %d cells", len(out))
	}
}

func TestIterator_ThresholdFilters(t *testing.T) {
	pq := &VectorPQ{codebook: makeCodebook(2, 4, 2), m: 2, ks: 4, dim: 4, dsub: 2}
	var pqBuf bytes.Buffer
	pq.WriteTo(&pqBuf)
	queryBuf := make([]byte, 4*4)
	for i := 0; i < 4; i++ {
		binary.BigEndian.PutUint32(queryBuf[i*4:], math.Float32bits(1.0))
	}

	// Set a threshold high enough to drop everything (max possible score
	// is bounded by codebook entries and query=1s).
	it, err := NewFromOptions(map[string]string{
		OptPQCodebook: base64.StdEncoding.EncodeToString(pqBuf.Bytes()),
		OptQuery:      base64.StdEncoding.EncodeToString(queryBuf),
		OptThreshold:  "1e30",
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		it.Offer([]byte("r"), nil, nil, nil, int64(i), []byte{byte(i), byte(i)})
	}
	if got := len(it.Drain()); got != 0 {
		t.Errorf("threshold should drop all; got %d", got)
	}
}

func TestIterator_BadOptionsReportClearError(t *testing.T) {
	cases := []struct {
		name string
		opts map[string]string
		want string
	}{
		{"missing query", map[string]string{OptPQCodebook: "abc"}, "missing"},
		{"missing pq", map[string]string{OptQuery: "abc"}, "missing"},
		{"bad b64", map[string]string{OptPQCodebook: "!!!", OptQuery: "abc"}, "decode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewFromOptions(tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v; want substring %q", err, tc.want)
			}
		})
	}
}
