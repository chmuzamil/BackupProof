// Package chunker implements FastCDC content-defined chunking with
// normalized chunk sizes. The gear table is derived from a per-repository
// secret seed so that chunk boundaries do not leak file fingerprints to
// someone who can only observe blob sizes.
package chunker

import (
	"bufio"
	"io"

	"lukechampine.com/blake3"
)

const (
	MinSize = 512 << 10 // 512 KiB
	AvgSize = 1 << 20   // 1 MiB
	MaxSize = 8 << 20   // 8 MiB

	// Normalized chunking level 2 (FastCDC paper): harder mask before the
	// average size, easier mask after it.
	maskS uint64 = 0x0003590703530000 // 22 bits set
	maskL uint64 = 0x0000d90003530000 // 18 bits set
)

// Params lets tests use small chunks. Production uses DefaultParams.
type Params struct {
	Min, Avg, Max int
}

var DefaultParams = Params{Min: MinSize, Avg: AvgSize, Max: MaxSize}

// GearTable derives the 256-entry gear table from a secret seed.
func GearTable(seed []byte) *[256]uint64 {
	var table [256]uint64
	h := blake3.New(32, seed)
	_, _ = h.Write([]byte("backupproof/chunker/gear/v1")) // hash writes never fail
	xof := h.XOF()
	var buf [8]byte
	for i := range table {
		if _, err := io.ReadFull(xof, buf[:]); err != nil {
			panic(err)
		}
		var v uint64
		for _, b := range buf {
			v = v<<8 | uint64(b)
		}
		table[i] = v
	}
	return &table
}

// Chunker splits a stream into content-defined chunks.
type Chunker struct {
	r     *bufio.Reader
	gear  *[256]uint64
	p     Params
	buf   []byte
	eof   bool
	maskS uint64
	maskL uint64
}

func New(r io.Reader, gear *[256]uint64, p Params) *Chunker {
	ms, ml := maskS, maskL
	if p.Avg != AvgSize {
		// Scale masks for non-default (test) sizes: bits = log2(avg) ± 2.
		bits := 0
		for v := p.Avg; v > 1; v >>= 1 {
			bits++
		}
		ms = spreadMask(bits + 2)
		ml = spreadMask(bits - 2)
	}
	return &Chunker{
		r:     bufio.NewReaderSize(r, 1<<20),
		gear:  gear,
		p:     p,
		buf:   make([]byte, 0, p.Max),
		maskS: ms,
		maskL: ml,
	}
}

func spreadMask(bits int) uint64 {
	if bits < 1 {
		bits = 1
	}
	var m uint64
	// Set every other high bit, mirroring the sparse masks of the paper.
	for i, n := 63, 0; i >= 0 && n < bits; i -= 2 {
		m |= 1 << uint(i)
		n++
	}
	return m
}

// Next returns the next chunk. The returned slice is only valid until the
// next call. It returns io.EOF when the stream is exhausted.
func (c *Chunker) Next() ([]byte, error) {
	c.buf = c.buf[:0]
	if c.eof {
		return nil, io.EOF
	}
	var fp uint64
	normal := c.p.Avg
	for len(c.buf) < c.p.Max {
		b, err := c.r.ReadByte()
		if err == io.EOF {
			c.eof = true
			break
		}
		if err != nil {
			return nil, err
		}
		c.buf = append(c.buf, b)
		n := len(c.buf)
		if n < c.p.Min {
			continue
		}
		fp = (fp << 1) + c.gear[b]
		if n < normal {
			if fp&c.maskS == 0 {
				return c.buf, nil
			}
		} else if fp&c.maskL == 0 {
			return c.buf, nil
		}
	}
	if len(c.buf) == 0 {
		return nil, io.EOF
	}
	return c.buf, nil
}
