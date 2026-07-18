package precomp

// 二値レンジコーダ(LZMA 型の適応二値算術符号)。
//
// JPEG の量子化 DCT 係数を、文脈モデルと組み合わせて算術符号化するために使う。
// Huffman は1シンボル最低1ビットの制約があるが、算術符号は確率に応じた
// 分数ビットで符号化できるため、適応確率+文脈モデルと合わせると Huffman を
// 明確に下回れる(lepton / packJPG / H.264 CABAC と同じ原理)。純Go・cgo不要。

// bitModel は1つの二値決定の適応確率(0..2048、初期 1024)。
type bitModel uint16

const modelInit bitModel = 1024

func newModels(n int) []bitModel {
	m := make([]bitModel, n)
	for i := range m {
		m[i] = modelInit
	}
	return m
}

const rcTopValue = 1 << 24

// rangeEncoder は算術符号のエンコーダ。
type rangeEncoder struct {
	low       uint64
	rng       uint32
	cache     byte
	cacheSize int64
	out       []byte
}

func newRangeEncoder() *rangeEncoder {
	return &rangeEncoder{rng: 0xFFFFFFFF, cacheSize: 1}
}

func (e *rangeEncoder) shiftLow() {
	if e.low < 0xFF000000 || e.low > 0xFFFFFFFF {
		c := e.cache
		for {
			e.out = append(e.out, byte(uint64(c)+(e.low>>32)))
			c = 0xFF
			e.cacheSize--
			if e.cacheSize == 0 {
				break
			}
		}
		e.cache = byte(e.low >> 24)
	}
	e.cacheSize++
	e.low = (e.low << 8) & 0xFFFFFFFF
}

// encodeBit は確率モデル p で1ビットを符号化し、p を適応更新する。
func (e *rangeEncoder) encodeBit(p *bitModel, bit int) {
	bound := (e.rng >> 11) * uint32(*p)
	if bit == 0 {
		e.rng = bound
		*p += (2048 - *p) >> 5
	} else {
		e.low += uint64(bound)
		e.rng -= bound
		*p -= *p >> 5
	}
	for e.rng < rcTopValue {
		e.rng <<= 8
		e.shiftLow()
	}
}

// encodeBitEq は確率固定 0.5 で1ビット符号化する(マンティッサ用)。
func (e *rangeEncoder) encodeBitEq(bit int) {
	e.rng >>= 1
	if bit != 0 {
		e.low += uint64(e.rng)
	}
	for e.rng < rcTopValue {
		e.rng <<= 8
		e.shiftLow()
	}
}

func (e *rangeEncoder) finish() []byte {
	for i := 0; i < 5; i++ {
		e.shiftLow()
	}
	return e.out
}

// rangeDecoder は算術符号のデコーダ。
type rangeDecoder struct {
	rng  uint32
	code uint32
	in   []byte
	pos  int
}

func newRangeDecoder(in []byte) *rangeDecoder {
	d := &rangeDecoder{rng: 0xFFFFFFFF, in: in, pos: 1} // 先頭バイト(常に0)を飛ばす
	for i := 0; i < 4; i++ {
		d.code = (d.code << 8) | uint32(d.next())
	}
	return d
}

func (d *rangeDecoder) next() byte {
	if d.pos < len(d.in) {
		b := d.in[d.pos]
		d.pos++
		return b
	}
	d.pos++
	return 0
}

func (d *rangeDecoder) decodeBit(p *bitModel) int {
	bound := (d.rng >> 11) * uint32(*p)
	var bit int
	if d.code < bound {
		d.rng = bound
		*p += (2048 - *p) >> 5
		bit = 0
	} else {
		d.code -= bound
		d.rng -= bound
		*p -= *p >> 5
		bit = 1
	}
	for d.rng < rcTopValue {
		d.rng <<= 8
		d.code = (d.code << 8) | uint32(d.next())
	}
	return bit
}

func (d *rangeDecoder) decodeBitEq() int {
	d.rng >>= 1
	var bit int
	if d.code >= d.rng {
		d.code -= d.rng
		bit = 1
	}
	for d.rng < rcTopValue {
		d.rng <<= 8
		d.code = (d.code << 8) | uint32(d.next())
	}
	return bit
}
