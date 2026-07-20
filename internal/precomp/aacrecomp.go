package precomp

// AAC ハフマン記号の二次算術符号化。walker(aacsyntax.go)を capture/rebuild
// の 2 シンクが駆動する。spectral/scalefactor 記号は符号帳ごとの適応二分木
// モデルで符号化し、逐語ビット・符号・エスケープ値はバイパス、エスケープ長
// (単項)は位置別モデルで符号化する。採用前にフレームごとに正準ハフマン
// 再符号化してバイト一致を検証し、非一致・非対応は原文退避へ落とす。

// aacModels は二次算術のモデル群(ストリーム内で持続)。
type aacModels struct {
	spec   [12][2][]cbModel
	specNB [12]int
	scf    []cbModel
	scfNB  int
	sign   cbModel
	escLen [9]cbModel
}

func aacBitLen(n int) int {
	b := 0
	for (1 << b) < n {
		b++
	}
	return b
}

func newCbModels(n int) []cbModel {
	s := make([]cbModel, n)
	for i := range s {
		s[i] = newCbModel()
	}
	return s
}

func newAACModels() *aacModels {
	m := &aacModels{sign: newCbModel()}
	for cb := 1; cb <= 11; cb++ {
		size := len(aacSpecTables[cb].Codes)
		nb := aacBitLen(size)
		m.specNB[cb] = nb
		m.spec[cb][0] = newCbModels(1 << nb)
		m.spec[cb][1] = newCbModels(1 << nb)
	}
	m.scfNB = aacBitLen(121)
	m.scf = newCbModels(1 << m.scfNB)
	for i := range m.escLen {
		m.escLen[i] = newCbModel()
	}
	return m
}

// aacEncSym / aacDecSym は適応二分木で 1 記号を符号化/復号する。
func aacEncSym(enc *rangeEncoder, models []cbModel, nb, sym int) {
	node := 1
	for i := nb - 1; i >= 0; i-- {
		bit := (sym >> uint(i)) & 1
		enc.encodeBitM(&models[node], bit)
		node = node*2 + bit
	}
}

func aacDecSym(dec *rangeDecoder, models []cbModel, nb int) int {
	node, sym := 1, 0
	for i := 0; i < nb; i++ {
		bit := dec.decodeBitM(&models[node])
		sym = sym*2 + bit
		node = node*2 + bit
	}
	return sym
}

// --- capture シンク(原ビット → 正準再符号化 + 任意で二次算術) ---
//
// enc == nil のとき算術符号化を行わず、正準再符号化のみ(検証パス用)。
type aacCaptureSink struct {
	r   *h264Reader
	cw  *h264Writer
	enc *rangeEncoder // nil で検証パス
	m   *aacModels
	bad bool
}

func (s *aacCaptureSink) bits(n int) uint32 {
	v, err := s.r.u(n)
	if err != nil {
		s.bad = true
		return 0
	}
	s.cw.u(v, n)
	if s.enc != nil {
		for i := n - 1; i >= 0; i-- {
			s.enc.encodeBitEq(int(v>>uint(i)) & 1)
		}
	}
	return v
}

func (s *aacCaptureSink) spec(cb, ctx int) int {
	sym, ok := aacSpecDec[cb].decode(s.r)
	if !ok {
		s.bad = true
		return 0
	}
	t := &aacSpecTables[cb]
	s.cw.u(uint32(t.Codes[sym]), int(t.Bits[sym]))
	if s.enc != nil {
		aacEncSym(s.enc, s.m.spec[cb][ctx], s.m.specNB[cb], sym)
	}
	return sym
}

func (s *aacCaptureSink) sign() int {
	b, err := s.r.u1()
	if err != nil {
		s.bad = true
		return 0
	}
	s.cw.u1(b)
	if s.enc != nil {
		s.enc.encodeBitM(&s.m.sign, int(b))
	}
	return int(b)
}

func (s *aacCaptureSink) esc() (int, int) {
	b := 0
	for {
		bit, err := s.r.u1()
		if err != nil {
			s.bad = true
			return 0, 0
		}
		if bit == 0 {
			break
		}
		b++
		if b > 8 {
			s.bad = true
			return 0, 0
		}
	}
	v, err := s.r.u(b + 4)
	if err != nil {
		s.bad = true
		return 0, 0
	}
	for i := 0; i < b; i++ {
		s.cw.u1(1)
	}
	s.cw.u1(0)
	s.cw.u(v, b+4)
	if s.enc != nil {
		for i := 0; i < b; i++ {
			s.enc.encodeBitM(&s.m.escLen[i], 1)
		}
		s.enc.encodeBitM(&s.m.escLen[b], 0)
		for i := b + 4 - 1; i >= 0; i-- {
			s.enc.encodeBitEq(int(v>>uint(i)) & 1)
		}
	}
	return b, int(v)
}

func (s *aacCaptureSink) scf() int {
	sym, ok := aacScfDec.decode(s.r)
	if !ok {
		s.bad = true
		return 0
	}
	s.cw.u(aacScfCodes[sym], int(aacScfBits[sym]))
	if s.enc != nil {
		aacEncSym(s.enc, s.m.scf, s.m.scfNB, sym)
	}
	return sym
}

func (s *aacCaptureSink) align() {
	for s.r.pos%8 != 0 {
		s.bits(1)
		if s.bad {
			return
		}
	}
}

func (s *aacCaptureSink) fail()        { s.bad = true }
func (s *aacCaptureSink) failed() bool { return s.bad }

// --- rebuild シンク(二次算術 → 正準再符号化) ---
type aacRebuildSink struct {
	dec *rangeDecoder
	cw  *h264Writer
	m   *aacModels
	bad bool
}

func (s *aacRebuildSink) bits(n int) uint32 {
	var v uint32
	for i := 0; i < n; i++ {
		v = v<<1 | uint32(s.dec.decodeBitEq())
	}
	s.cw.u(v, n)
	return v
}

func (s *aacRebuildSink) spec(cb, ctx int) int {
	sym := aacDecSym(s.dec, s.m.spec[cb][ctx], s.m.specNB[cb])
	t := &aacSpecTables[cb]
	if sym >= len(t.Codes) {
		s.bad = true
		return 0
	}
	s.cw.u(uint32(t.Codes[sym]), int(t.Bits[sym]))
	return sym
}

func (s *aacRebuildSink) sign() int {
	b := s.dec.decodeBitM(&s.m.sign)
	s.cw.u1(uint32(b))
	return b
}

func (s *aacRebuildSink) esc() (int, int) {
	b := 0
	for b <= 8 && s.dec.decodeBitM(&s.m.escLen[b]) == 1 {
		b++
	}
	if b > 8 {
		s.bad = true
		return 0, 0
	}
	v := 0
	for i := 0; i < b+4; i++ {
		v = v<<1 | s.dec.decodeBitEq()
	}
	for i := 0; i < b; i++ {
		s.cw.u1(1)
	}
	s.cw.u1(0)
	s.cw.u(uint32(v), b+4)
	return b, v
}

func (s *aacRebuildSink) scf() int {
	sym := aacDecSym(s.dec, s.m.scf, s.m.scfNB)
	if sym >= 121 {
		s.bad = true
		return 0
	}
	s.cw.u(aacScfCodes[sym], int(aacScfBits[sym]))
	return sym
}

func (s *aacRebuildSink) align() {
	for s.cw.nbit%8 != 0 {
		s.bits(1)
		if s.bad {
			return
		}
	}
}

func (s *aacRebuildSink) fail()        { s.bad = true }
func (s *aacRebuildSink) failed() bool { return s.bad }
