// Package chunker implements FastCDC content-defined chunking with
// normalized chunk sizes. The gear table is derived from a per-repository
// secret seed so that chunk boundaries do not leak file fingerprints to
// someone who can only observe blob sizes.
package chunker

import (
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
	r          io.Reader
	gear       *[256]uint64
	p          Params
	buf        []byte // read-ahead: unread data is buf[start:end]
	start, end int
	eof        bool
	maskS      uint64
	maskL      uint64
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
		r:     r,
		gear:  gear,
		p:     p,
		buf:   make([]byte, 2*p.Max),
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
	if err := c.fill(); err != nil {
		return nil, err
	}
	data := c.buf[c.start:c.end]
	if len(data) == 0 {
		return nil, io.EOF
	}
	n := c.cut(data)
	c.start += n
	return data[:n:n], nil
}

// fill makes at least Max bytes available, or everything left in the stream.
func (c *Chunker) fill() error {
	if c.end-c.start >= c.p.Max || c.eof {
		return nil
	}
	c.end = copy(c.buf, c.buf[c.start:c.end])
	c.start = 0
	for c.end < c.p.Max && !c.eof {
		n, err := c.r.Read(c.buf[c.end:])
		c.end += n
		if err == io.EOF {
			c.eof = true
		} else if err != nil {
			return err
		}
	}
	return nil
}

// cut returns the length of the next chunk at the start of data: the first
// position at or after Min where the gear fingerprint matches the mask (the
// harder mask before Avg, the easier one after), or Max, or all of data at
// the end of the stream. The fingerprint starts at Min, as in FastCDC.
func (c *Chunker) cut(data []byte) int {
	limit := min(len(data), c.p.Max)
	if limit < c.p.Min {
		return limit
	}
	gear := c.gear
	var fp uint64
	i := max(c.p.Min-1, 0)
	for end := min(c.p.Avg-1, limit); i < end; i++ {
		fp = (fp << 1) + gear[data[i]]
		if fp&c.maskS == 0 {
			return i + 1
		}
	}
	for ; i < limit; i++ {
		fp = (fp << 1) + gear[data[i]]
		if fp&c.maskL == 0 {
			return i + 1
		}
	}
	return limit
}
