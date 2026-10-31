package chunker

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"io"
	"testing"
	"testing/iotest"
)

// reference is the original byte-at-a-time chunker. Chunk boundaries decide
// which data a repository already has, so the faster Chunker must cut at
// exactly the same places, or existing repositories would store everything
// again.
func reference(data []byte, gear *[256]uint64, p Params, ms, ml uint64) [][]byte {
	r := bufio.NewReader(bytes.NewReader(data))
	var out [][]byte
	for {
		var buf []byte
		var fp uint64
		for len(buf) < p.Max {
			b, err := r.ReadByte()
			if err != nil {
				break
			}
			buf = append(buf, b)
			n := len(buf)
			if n < p.Min {
				continue
			}
			fp = (fp << 1) + gear[b]
			if n < p.Avg {
				if fp&ms == 0 {
					break
				}
			} else if fp&ml == 0 {
				break
			}
		}
		if len(buf) == 0 {
			return out
		}
		out = append(out, buf)
	}
}

func chunks(t testing.TB, r io.Reader, gear *[256]uint64, p Params) [][]byte {
	c := New(r, gear, p)
	var out [][]byte
	for {
		b, err := c.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, append([]byte(nil), b...))
	}
}

func TestSameBoundariesAsReference(t *testing.T) {
	gear := GearTable(bytes.Repeat([]byte{7}, 32))
	small := Params{Min: 4 << 10, Avg: 16 << 10, Max: 64 << 10}
	data := make([]byte, 3<<20)
	rand.Read(data)
	// Long runs of identical bytes exercise the Max cut.
	copy(data[1<<20:], make([]byte, 300<<10))
	for _, tc := range []struct {
		name string
		p    Params
		data []byte
	}{
		{"default", DefaultParams, data},
		{"small", small, data},
		{"tiny input", small, data[:3000]},
		{"empty", small, nil},
		{"exactly max", small, data[:small.Max]},
	} {
		c := New(nil, gear, tc.p)
		want := reference(tc.data, gear, tc.p, c.maskS, c.maskL)
		for _, rd := range []struct {
			name string
			r    io.Reader
		}{
			{"whole", bytes.NewReader(tc.data)},
			{"one byte at a time", iotest.OneByteReader(bytes.NewReader(tc.data))},
			{"half reads", iotest.HalfReader(bytes.NewReader(tc.data))},
		} {
			got := chunks(t, rd.r, gear, tc.p)
			if len(got) != len(want) {
				t.Fatalf("%s/%s: %d chunks, want %d", tc.name, rd.name, len(got), len(want))
			}
			for i := range got {
				if !bytes.Equal(got[i], want[i]) {
					t.Fatalf("%s/%s: chunk %d differs (%d vs %d bytes)", tc.name, rd.name, i, len(got[i]), len(want[i]))
				}
			}
		}
	}
}

func BenchmarkChunker(b *testing.B) {
	gear := GearTable(bytes.Repeat([]byte{7}, 32))
	data := make([]byte, 64<<20)
	rand.Read(data)
	b.SetBytes(int64(len(data)))
	for i := 0; i < b.N; i++ {
		c := New(bytes.NewReader(data), gear, DefaultParams)
		for {
			if _, err := c.Next(); err != nil {
				break
			}
		}
	}
}
