// Package ivfpq is a Go port of the Java IVF-PQ codebook + ADC iterator
// that lives in core/.../graph/ann/. The port is wire-compatible with
// the Java serializer (big-endian float32 + Java DataOutput int32
// headers); shoal uses it to run the IvfPqDistanceIterator natively
// inside a scan instead of round-tripping raw PQ codes back to the
// Java client.
//
// Java sources for reference:
//   - core/.../graph/ann/VectorPQ.java
//   - core/.../graph/ann/IvfPqDistanceIterator.java
//   - core/.../graph/ann/IvfPqTable.java (column constants)
package ivfpq

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
)

// FormatVersion of the VectorPQ serialization. Mirrors
// VectorPQ.FORMAT_VERSION on the Java side. Bumping requires a
// coordinated change in both writers + readers.
const FormatVersion int32 = 1

// VectorPQ holds a trained PQ codebook plus the inner-product table
// machinery needed at query time.
//
// codebook[m][ks][dsub] is the literal codebook: M subspaces × Ks
// centroids × dsub-dim slices. dsub = dim / m.
type VectorPQ struct {
	codebook        [][][]float32
	m               int
	ks              int
	dim             int
	dsub            int
	codebookVersion int32
	embeddingSpace  string
}

// FromBytes parses the Java-serialized VectorPQ wire format:
//
//	int32 FORMAT_VERSION
//	int32 codebookVersion
//	int32 M
//	int32 Ks
//	int32 dim
//	float32[M*Ks*dsub]   (big-endian)
//
// All ints + floats are big-endian (Java DataOutput convention).
func FromBytes(b []byte) (*VectorPQ, error) {
	r := bytes.NewReader(b)
	var format int32
	if err := binary.Read(r, binary.BigEndian, &format); err != nil {
		return nil, fmt.Errorf("read FORMAT_VERSION: %w", err)
	}
	if format != FormatVersion {
		return nil, fmt.Errorf("unsupported VectorPQ format: %d", format)
	}
	var codebookVersion, m, ks, dim int32
	for _, field := range []struct {
		name string
		out  *int32
	}{{"codebookVersion", &codebookVersion}, {"m", &m}, {"ks", &ks}, {"dim", &dim}} {
		if err := binary.Read(r, binary.BigEndian, field.out); err != nil {
			return nil, fmt.Errorf("read %s: %w", field.name, err)
		}
	}
	if m <= 0 || ks <= 0 || ks > 256 || dim <= 0 {
		return nil, fmt.Errorf("invalid VectorPQ header: m=%d ks=%d dim=%d", m, ks, dim)
	}
	if dim%m != 0 {
		return nil, fmt.Errorf("dim %d not divisible by M %d", dim, m)
	}
	dsub := dim / m

	codebook := make([][][]float32, m)
	for i := int32(0); i < m; i++ {
		sub := make([][]float32, ks)
		for c := int32(0); c < ks; c++ {
			entry := make([]float32, dsub)
			if err := binary.Read(r, binary.BigEndian, entry); err != nil {
				return nil, fmt.Errorf("read codebook[%d][%d]: %w", i, c, err)
			}
			sub[c] = entry
		}
		codebook[i] = sub
	}
	if r.Len() != 0 {
		return nil, fmt.Errorf("VectorPQ trailing bytes: %d", r.Len())
	}
	return &VectorPQ{
		codebook:        codebook,
		m:               int(m),
		ks:              int(ks),
		dim:             int(dim),
		dsub:            int(dsub),
		codebookVersion: codebookVersion,
	}, nil
}

// WriteTo writes the VectorPQ in Java DataOutput format. Mostly used
// for tests that round-trip against captured Java bytes.
func (p *VectorPQ) WriteTo(w io.Writer) (int64, error) {
	var n int64
	wr := func(v any) error {
		if err := binary.Write(w, binary.BigEndian, v); err != nil {
			return err
		}
		n += int64(binary.Size(v))
		return nil
	}
	if err := wr(FormatVersion); err != nil {
		return n, err
	}
	if err := wr(p.codebookVersion); err != nil {
		return n, err
	}
	if err := wr(int32(p.m)); err != nil {
		return n, err
	}
	if err := wr(int32(p.ks)); err != nil {
		return n, err
	}
	if err := wr(int32(p.dim)); err != nil {
		return n, err
	}
	for i := 0; i < p.m; i++ {
		for c := 0; c < p.ks; c++ {
			if err := wr(p.codebook[i][c]); err != nil {
				return n, err
			}
		}
	}
	return n, nil
}

// M returns the number of PQ subspaces (= byte length of an encoded
// vector).
func (p *VectorPQ) M() int { return p.m }

// Ks returns the number of centroids per subspace.
func (p *VectorPQ) Ks() int { return p.ks }

// Dim returns the original (pre-PQ) vector dimension.
func (p *VectorPQ) Dim() int { return p.dim }

// CodeLength is the byte length of an encoded vector — alias for M.
func (p *VectorPQ) CodeLength() int { return p.m }

// CodebookVersion is a tag stamped into the codebook at training time
// so callers can detect stale codebooks vs ones a new ingest stamped.
func (p *VectorPQ) CodebookVersion() int32 { return p.codebookVersion }

func (p *VectorPQ) WithEmbeddingSpace(identity string) (*VectorPQ, error) {
	if err := embeddingspace.Has(identity).Validate(); err != nil {
		return nil, err
	}
	clone := *p
	clone.embeddingSpace = identity
	return &clone, nil
}

func (p *VectorPQ) EmbeddingSpaceIdentity() (string, bool) {
	if p == nil || p.embeddingSpace == "" {
		return "", false
	}
	return p.embeddingSpace, true
}

func (p *VectorPQ) EnsureQuerySpace(identity string) error {
	if p == nil || p.embeddingSpace == "" {
		return nil
	}
	return embeddingspace.EnsureSameIdentity("compare product-quantized vectors", p.embeddingSpace, identity)
}

// InnerProductTable precomputes, for each subspace s and each codebook
// entry c, the inner product between query[s*dsub:(s+1)*dsub] and
// codebook[s][c]. Result is [m][ks]. Caller usually does this once per
// query and reuses across thousands of Dot calls.
func (p *VectorPQ) InnerProductTable(query []float32) ([][]float32, error) {
	if len(query) != p.dim {
		return nil, fmt.Errorf("query dim %d != PQ dim %d", len(query), p.dim)
	}
	out := make([][]float32, p.m)
	for s := 0; s < p.m; s++ {
		off := s * p.dsub
		row := make([]float32, p.ks)
		sub := p.codebook[s]
		for c := 0; c < p.ks; c++ {
			row[c] = dotSlice(query, off, sub[c])
		}
		out[s] = row
	}
	return out, nil
}

func (p *VectorPQ) InnerProductTableInSpace(query []float32, identity string) ([][]float32, error) {
	if err := p.EnsureQuerySpace(identity); err != nil {
		return nil, err
	}
	return p.InnerProductTable(query)
}

// Dot scores a PQ-encoded vector against the query whose
// InnerProductTable is t. O(M) lookups + adds — no float multiplies in
// the hot loop. Returns 0 with no error on len mismatch (the Java
// version throws; shoal's scan path prefers swallowing rather than
// failing the whole batch on a single corrupt cell).
func (p *VectorPQ) Dot(code []byte, t [][]float32) float32 {
	if len(code) != p.m {
		return 0
	}
	var sum float32
	for s := 0; s < p.m; s++ {
		sum += t[s][int(code[s])&0xff] // unsigned-byte → int index
	}
	return sum
}

// Bytes serialises the VectorPQ via WriteTo and returns the resulting bytes.
func (p *VectorPQ) Bytes() ([]byte, error) {
	var buf bytes.Buffer
	if _, err := p.WriteTo(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func dotSlice(full []float32, off int, slice []float32) float32 {
	var s float32
	for i := 0; i < len(slice); i++ {
		s += full[off+i] * slice[i]
	}
	return s
}
