// Package tsz implement time-series compression
/*

http://www.vldb.org/pvldb/vol8/p1816-teller.pdf

*/

package tsz

import (
	"bytes"
	"math"

	"github.com/dgryski/go-bits"
	"github.com/dgryski/go-bitstream"
)

type Series struct {

	// TODO(dgryski): timestamps in the paper are uint64

	t0     uint32
	tDelta uint32
	t      uint32
	val    float64

	leading  uint64
	trailing uint64

	buf bytes.Buffer
	bw  *bitstream.BitWriter

	finished bool
}

func New(t0 uint32) *Series {
	s := Series{
		t0:      t0,
		leading: ^uint64(0),
	}

	s.bw = bitstream.NewWriter(&s.buf)

	// block header
	s.bw.WriteBits(uint64(t0), 32)

	return &s

}

func (s *Series) Bytes() []byte {
	return s.buf.Bytes()
}

func (s *Series) Finish() {

	if !s.finished {
		// write an end-of-stream record
		s.bw.WriteBits(0x0f, 4)
		s.bw.WriteBits(0xffffffff, 32)
		s.bw.WriteBit(bitstream.Zero)
		s.bw.Flush(bitstream.Zero)
		s.finished = true
	}
}

func (s *Series) Push(t uint32, v float64) {

	if s.t == 0 {
		// first point
		s.t = t
		s.val = v
		s.tDelta = t - s.t0
		s.bw.WriteBits(uint64(s.tDelta), 14)
		s.bw.WriteBits(math.Float64bits(v), 64)
		return
	}

	tDelta := t - s.t
	dod := int32(tDelta - s.tDelta)

	switch {
	case dod == 0:
		s.bw.WriteBit(bitstream.Zero)
	case -63 <= dod && dod <= 64:
		s.bw.WriteBits(0x02, 2) // '10'
		s.bw.WriteBits(uint64(dod), 7)
	case -255 <= dod && dod <= 256:
		s.bw.WriteBits(0x06, 3) // '110'
		s.bw.WriteBits(uint64(dod), 9)
	case -2047 <= dod && dod <= 2048:
		s.bw.WriteBits(0x0e, 4) // '1110'
		s.bw.WriteBits(uint64(dod), 12)
	default:
		s.bw.WriteBits(0x0f, 4) // '1111'
		s.bw.WriteBits(uint64(dod), 32)
	}

	vDelta := math.Float64bits(v) ^ math.Float64bits(s.val)

	if vDelta == 0 {
		s.bw.WriteBit(bitstream.Zero)
	} else {
		s.bw.WriteBit(bitstream.One)

		leading := bits.Clz(vDelta)
		trailing := bits.Ctz(vDelta)

		// TODO(dgryski): check if it's 'cheaper' to reset the leading/trailing bits instead
		if s.leading != ^uint64(0) && leading >= s.leading && trailing >= s.trailing {
			s.bw.WriteBit(bitstream.Zero)
			s.bw.WriteBits(vDelta>>s.trailing, 64-int(s.leading)-int(s.trailing))
		} else {
			s.leading, s.trailing = leading, trailing

			s.bw.WriteBit(bitstream.One)
			s.bw.WriteBits(leading, 5)

			sigbits := 64 - leading - trailing
			s.bw.WriteBits(sigbits, 6)
			s.bw.WriteBits(vDelta>>trailing, int(sigbits))
		}
	}

	s.tDelta = tDelta
	s.t = t
	s.val = v

}

func (s *Series) Iter() *Iter {
	iter, _ := NewIterator(s.buf.Bytes())
	return iter
}

type Iter struct {
	t0 uint32

	tDelta uint32
	t      uint32
	val    float64

	leading  uint64
	trailing uint64

	br *bitstream.BitReader

	b []byte

	finished bool

	err error
}

func NewIterator(b []byte) (*Iter, error) {
	br := bitstream.NewReader(bytes.NewReader(b))

	t0, err := br.ReadBits(32)
	if err != nil {
		return nil, err
	}

	return &Iter{
		t0: uint32(t0),
		br: br,
		b:  b,
	}, nil
}

func (it *Iter) Next() bool {

	if it.err != nil || it.finished {
		return false
	}

	if it.t == 0 {
		// read first t and v
		tDelta, err := it.br.ReadBits(14)
		if err != nil {
			it.err = err
			return false
		}
		it.tDelta = uint32(tDelta)
		it.t = it.t0 + it.tDelta
		v, err := it.br.ReadBits(64)
		if err != nil {
			it.err = err
			return false
		}

		it.val = math.Float64frombits(v)

		return true
	}

	// read delta-of-delta
	var d byte
	for i := 0; i < 4; i++ {
		d <<= 1
		bit, err := it.br.ReadBit()
		if err != nil {
			it.err = err
			return false
		}
		if bit == bitstream.Zero {
			break
		}
		d |= 1
	}

	var dod int32
	var sz uint
	switch d {
	case 0x00:
		// dod == 0
	case 0x02:
		sz = 7
	case 0x06:
		sz = 9
	case 0x0e:
		sz = 12
	case 0x0f:
		bits, err := it.br.ReadBits(32)
		if err != nil {
			it.err = err
			return false
		}

		// end of stream
		if bits == 0xffffffff {
			it.finished = true
			return false
		}

		dod = int32(bits)
	}

	if sz != 0 {
		bits, err := it.br.ReadBits(int(sz))
		if err != nil {
			it.err = err
			return false
		}
		if bits > (1 << (sz - 1)) {
			// or something
			bits = bits - (1 << sz)
		}
		dod = int32(bits)
	}

	tDelta := it.tDelta + uint32(dod)

	it.tDelta = tDelta
	it.t = it.t + it.tDelta

	// read compressed value
	d = 0
	bit, err := it.br.ReadBit()
	if err != nil {
		it.err = err
		return false
	}

	if bit == bitstream.Zero {
		// it.val = it.val
	} else {
		bit, err := it.br.ReadBit()
		if err != nil {
			it.err = err
			return false
		}
		if bit == bitstream.Zero {
			// reuse leading/trailing zero bits
			// it.leading, it.trailing = it.leading, it.trailing
		} else {
			bits, err := it.br.ReadBits(5)
			if err != nil {
				it.err = err
				return false
			}
			it.leading = bits

			bits, err = it.br.ReadBits(6)
			if err != nil {
				it.err = err
				return false
			}
			mbits := bits
			it.trailing = 64 - it.leading - mbits
		}

		mbits := int(64 - it.leading - it.trailing)
		bits, err := it.br.ReadBits(mbits)
		if err != nil {
			it.err = err
			return false
		}
		vbits := math.Float64bits(it.val)
		vbits ^= (bits << it.trailing)
		it.val = math.Float64frombits(vbits)
	}

	return true
}

func (it *Iter) Values() (uint32, float64) {
	return it.t, it.val
}

func (it *Iter) Err() error {
	return it.err
}
