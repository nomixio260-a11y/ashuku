package precomp

// CABAC 可逆再圧縮の二次コーダ。
//
// CABAC の文脈適応二値算術符号は 2003 年設計の 64 状態 FSM で、確率適応が
// 粗く、かつ**スライスごとに文脈を初期化**する。ここではバイト厳密に取り出した
// ビン列(cabacISlice が駆動)を、より高精度(11bit)で**スライス跨ぎに持続**
// する二次算術符号へ載せ替える。文脈コード化ビンでの推定改善と、初期化
// オーバヘッドの回収が圧縮源。バイパスビン(等確率)は素通し。
//
// capture: 原 CABAC を復号しつつ各ビンを二次符号へ(ctxIdx をキーに)。
// rebuild: 二次符号から復号→ CABAC へ再符号化(原バイトを厳密再生)。
// 両経路とも cabacISlice が同じ ctxIdx 列を同順に生成するので lockstep。

// --- 二次二値レンジコーダ(LZMA 系、11bit 適応ビットモデル) ---

const (
	rcTopBits    = 24
	rcTop        = 1 << rcTopBits
	rcModelBits  = 11
	rcModelTotal = 1 << rcModelBits
	rcMoveBits   = 5
	rcProbInit   = rcModelTotal / 2
)

type rcEncoder struct {
	low       uint64
	rng       uint32
	cache     byte
	cacheSize int64
	out       []byte
}

func newRcEncoder() *rcEncoder { return &rcEncoder{rng: 0xFFFFFFFF, cacheSize: 1} }

func (e *rcEncoder) shiftLow() {
	if uint32(e.low>>32) != 0 || e.low < 0xFF000000 {
		temp := e.cache
		for {
			e.out = append(e.out, byte(uint64(temp)+(e.low>>32)))
			temp = 0xFF
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

func (e *rcEncoder) encodeBit(prob *uint16, bit int) {
	bound := (e.rng >> rcModelBits) * uint32(*prob)
	if bit == 0 {
		e.rng = bound
		*prob += (rcModelTotal - *prob) >> rcMoveBits
	} else {
		e.low += uint64(bound)
		e.rng -= bound
		*prob -= *prob >> rcMoveBits
	}
	for e.rng < rcTop {
		e.rng <<= 8
		e.shiftLow()
	}
}

// encodeDirect は等確率1ビン(バイパス)。
func (e *rcEncoder) encodeDirect(bit int) {
	e.rng >>= 1
	if bit != 0 {
		e.low += uint64(e.rng)
	}
	for e.rng < rcTop {
		e.rng <<= 8
		e.shiftLow()
	}
}

func (e *rcEncoder) flush() {
	for i := 0; i < 5; i++ {
		e.shiftLow()
	}
}

type rcDecoder struct {
	code uint32
	rng  uint32
	in   []byte
	pos  int
}

func newRcDecoder(in []byte) *rcDecoder {
	d := &rcDecoder{rng: 0xFFFFFFFF, in: in}
	d.pos = 1 // 先頭バイトは 0(エンコーダの cache 初期）
	for i := 0; i < 4; i++ {
		d.code = d.code<<8 | uint32(d.readByte())
	}
	return d
}

func (d *rcDecoder) readByte() byte {
	if d.pos < len(d.in) {
		b := d.in[d.pos]
		d.pos++
		return b
	}
	d.pos++
	return 0
}

func (d *rcDecoder) decodeBit(prob *uint16) int {
	bound := (d.rng >> rcModelBits) * uint32(*prob)
	var bit int
	if d.code < bound {
		d.rng = bound
		*prob += (rcModelTotal - *prob) >> rcMoveBits
		bit = 0
	} else {
		d.code -= bound
		d.rng -= bound
		*prob -= *prob >> rcMoveBits
		bit = 1
	}
	for d.rng < rcTop {
		d.rng <<= 8
		d.code = d.code<<8 | uint32(d.readByte())
	}
	return bit
}

func (d *rcDecoder) decodeDirect() int {
	d.rng >>= 1
	var bit int
	if d.code >= d.rng {
		d.code -= d.rng
		bit = 1
	}
	for d.rng < rcTop {
		d.rng <<= 8
		d.code = d.code<<8 | uint32(d.readByte())
	}
	return bit
}

// --- 二次モデル(全 CABAC 文脈 + terminate、スライス跨ぎ持続) ---

type cabacSecModel struct {
	ctx  [1024]uint16
	term uint16
}

func newCabacSecModel() *cabacSecModel {
	m := &cabacSecModel{term: rcProbInit}
	for i := range m.ctx {
		m.ctx[i] = rcProbInit
	}
	return m
}

// --- capture シンク(原CABAC→二次符号) ---

type cabacCaptureSink struct {
	d   *cabacDecoder
	st  *[1024]uint8
	enc *rcEncoder
	m   *cabacSecModel
}

func (s *cabacCaptureSink) decision(ctx int) int {
	bin := s.d.decodeDecision(&s.st[ctx])
	s.enc.encodeBit(&s.m.ctx[ctx], bin)
	return bin
}
func (s *cabacCaptureSink) bypass() int {
	bin := s.d.decodeBypass()
	s.enc.encodeDirect(bin)
	return bin
}
func (s *cabacCaptureSink) terminate() int {
	bin := s.d.decodeTerminate()
	s.enc.encodeBit(&s.m.term, bin)
	return bin
}
func (s *cabacCaptureSink) failed() bool { return s.d.err }

// intraPCM: 生画素バイトを二次符号へ素通し(等確率8bit)しつつ復号器再初期化。
func (s *cabacCaptureSink) intraPCM(mbSize int) bool {
	c := s.d.r.pos - 9 + cabacPCMAlign
	pcmStart := (c + 7) &^ 7
	pcmEnd := pcmStart + mbSize*8
	if pcmEnd > len(s.d.r.b)*8 {
		s.d.err = true
		return false
	}
	for bit := pcmStart; bit < pcmEnd; bit++ {
		v := int(s.d.r.b[bit>>3]>>(7-uint(bit&7))) & 1
		s.enc.encodeDirect(v)
	}
	s.d.reinitAt(pcmEnd)
	return !s.d.err
}

// --- rebuild シンク(二次符号→CABAC 再符号化) ---

type cabacRebuildSink struct {
	dec    *rcDecoder
	enc    *cabacEncoder
	st     *[1024]uint8
	m      *cabacSecModel
	pcmErr bool
}

func (s *cabacRebuildSink) decision(ctx int) int {
	bin := s.dec.decodeBit(&s.m.ctx[ctx])
	s.enc.encodeDecision(&s.st[ctx], bin)
	return bin
}
func (s *cabacRebuildSink) bypass() int {
	bin := s.dec.decodeDirect()
	s.enc.encodeBypass(bin)
	return bin
}
func (s *cabacRebuildSink) terminate() int {
	bin := s.dec.decodeBit(&s.m.term)
	s.enc.encodeTerminate(bin)
	return bin
}
func (s *cabacRebuildSink) failed() bool { return s.pcmErr }

// intraPCM: I_PCM を含むスライスは rebuild 経路では非対応(往復検証で素通しへ退避)。
func (s *cabacRebuildSink) intraPCM(mbSize int) bool {
	s.pcmErr = true
	return false
}
