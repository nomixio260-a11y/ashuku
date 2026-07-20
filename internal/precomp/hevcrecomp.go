package precomp

// HEVC CABAC の可逆再圧縮シンク(capture/tee/rebuild)。
//
// H.264 側(cabacrecomp.go)と同型: capture は原 CABAC を復号しながら
// 二次適応算術(cbModel 二速混合)へ載せ替え、同時に正準 CABAC 再符号化を
// 並走させて原文との共通接頭辞を求める。HEVC 特有なのはサブストリーム
// (WPP 行)がバイト整列した独立区間である点で、[delta][tail] の照合を
// サブストリーム単位で行う。

// hevcSecModel は二次算術の文脈確率(スライス跨ぎで持続)。
//
// 文脈ビンは「学習型 cbModel(文脈ごと)」と「CABAC 状態が意味する確率
// (決定的な副情報)」を 1:1 で混合して符号化する。混合により立ち上がりは
// 常に CABAC 自身の推定から出発し、データが貯まるほど学習側の上積みが利く。
type hevcSecModel struct {
	ctx  []cbModel
	term cbModel
	byp  []cbModel // クラス付きバイパスの文脈
}

func newHEVCSecModel() *hevcSecModel {
	m := &hevcSecModel{ctx: make([]cbModel, hevcNumContexts), term: cbModel{2016, 2016},
		byp: make([]cbModel, hevcBypNumCls)}
	for i := range m.ctx {
		m.ctx[i] = newCbModel()
	}
	for i := range m.byp {
		m.byp[i] = newCbModel()
	}
	return m
}

// hevcStateP0 は s7 状態が意味する P(bin=0) を 4096 スケールで返す。
func hevcStateP0(s uint8) uint32 {
	p := hevcLPS2048[s>>1] * 2
	if s&1 == 0 {
		p = 4096 - p
	}
	if p < 64 {
		p = 64
	}
	if p > 4032 {
		p = 4032
	}
	return p
}

// encodeBitMix / decodeBitMix は学習確率と状態確率の 1:1 混合で 1 ビンを
// 符号化する(モデル更新は学習側のみ)。
func hevcEncodeBitMix(e *rangeEncoder, m *cbModel, pState uint32, bit int) {
	p := (m.prob() + pState) >> 1
	bound := (e.rng >> 12) * p
	if bit == 0 {
		e.rng = bound
	} else {
		e.low += uint64(bound)
		e.rng -= bound
	}
	m.update(bit)
	for e.rng < rcTopValue {
		e.rng <<= 8
		e.shiftLow()
	}
}

func hevcDecodeBitMix(d *rangeDecoder, m *cbModel, pState uint32) int {
	p := (m.prob() + pState) >> 1
	bound := (d.rng >> 12) * p
	var bit int
	if d.code < bound {
		d.rng = bound
	} else {
		d.code -= bound
		d.rng -= bound
		bit = 1
	}
	m.update(bit)
	for d.rng < rcTopValue {
		d.rng <<= 8
		d.code = (d.code << 8) | uint32(d.next())
	}
	return bit
}

// hevcLPS2048 は CABAC 状態 σ の LPS 確率(2048 スケール)。
// p(σ) = 0.5·α^σ(α≈0.949217)を整数反復で決定的に計算する。
var hevcLPS2048 = func() [64]uint32 {
	var t [64]uint32
	v := uint32(1024 << 16) // 0.5 を 2048<<16 スケールで
	for i := 0; i < 64; i++ {
		t[i] = v >> 16
		v = uint32(uint64(v) * 62215 >> 16)
	}
	return t
}()

// hevcSubFix は 1 サブストリームの原文照合結果。
// rebuild は canon[:len(canon)-delta] + tail で原文を復元する。
type hevcSubFix struct {
	delta int
	tail  []byte
}

// --- tee シンク(capture) ---

type hevcTeeSink struct {
	d      *cabacDecoder
	stD    [hevcNumContexts]uint8
	savedD [hevcNumContexts]uint8
	rc     *rangeEncoder
	m      *hevcSecModel
	w      *h264Writer
	ce     *cabacEncoder
	stE    [hevcNumContexts]uint8
	savedE [hevcNumContexts]uint8

	body     []byte // スライス RBSP ボディ(NAL ヘッダ後)
	subStart int    // 現サブストリームの開始バイト(body 基準)
	fixes    []hevcSubFix
	bad      bool
}

func (s *hevcTeeSink) decision(ctx int) int {
	ps := hevcStateP0(s.stD[ctx])
	bin := s.d.decodeDecision(&s.stD[ctx])
	hevcEncodeBitMix(s.rc, &s.m.ctx[ctx], ps, bin)
	s.ce.encodeDecision(&s.stE[ctx], bin)
	return bin
}

func (s *hevcTeeSink) bypassCls(cls int) int {
	bin := s.d.decodeBypass()
	s.rc.encodeBitM(&s.m.byp[cls], bin)
	s.ce.encodeBypass(bin)
	return bin
}

func (s *hevcTeeSink) bypass() int {
	bin := s.d.decodeBypass()
	s.rc.encodeBitEq(bin)
	s.ce.encodeBypass(bin)
	return bin
}

func (s *hevcTeeSink) terminate() int {
	bin := s.d.decodeTerminate()
	s.rc.encodeBitM(&s.m.term, bin)
	s.ce.encodeTerminate(bin)
	return bin
}

func (s *hevcTeeSink) failed() bool { return s.d.err || s.bad }

// intraPCM は PCM 生バイト(二次へ素通し)+復号器再初期化+正準側挿入。
func (s *hevcTeeSink) intraPCM(nBytes int) bool {
	c := s.d.r.pos - 9
	start := (c + 7) &^ 7
	end := start + nBytes*8
	if end > len(s.d.r.b)*8 || start%8 != 0 {
		s.d.err = true
		return false
	}
	data := s.d.r.b[start/8 : end/8]
	for _, b := range data {
		for k := 7; k >= 0; k-- {
			s.rc.encodeBitEq(int(b>>uint(k)) & 1)
		}
	}
	s.d.reinitAt(end)
	s.ce.pcmInsert(data)
	return !s.d.err
}

func (s *hevcTeeSink) initCtx(sliceQP, initType int) {
	hevcInitStates(&s.stD, sliceQP, initType)
	hevcInitStates(&s.stE, sliceQP, initType)
}

func (s *hevcTeeSink) saveCtx() {
	s.savedD = s.stD
	s.savedE = s.stE
}

func (s *hevcTeeSink) loadCtx() {
	s.stD = s.savedD
	s.stE = s.savedE
}

// finishSub は現サブストリーム [subStart, endByte) の正準出力を原文と照合し
// [delta][tail] を記録する。正準側の直近 terminate(1) は flush 済み。
func (s *hevcTeeSink) finishSub(endByte int) bool {
	if endByte < s.subStart || endByte > len(s.body) {
		s.bad = true
		return false
	}
	for s.w.nbit%8 != 0 {
		s.w.u1(0)
	}
	canon := s.w.b
	orig := s.body[s.subStart:endByte]
	match := 0
	for match < len(canon) && match < len(orig) && canon[match] == orig[match] {
		match++
	}
	delta := len(canon) - match
	tail := orig[match:]
	if delta < 0 || delta > 255 || len(tail) > 64 {
		s.bad = true
		return false
	}
	s.fixes = append(s.fixes, hevcSubFix{delta: delta, tail: append([]byte(nil), tail...)})
	// 次サブストリームへ: 正準側を新規に開始
	s.w = &h264Writer{}
	s.ce = newCabacEncoder(s.w)
	s.subStart = endByte
	return true
}

// reinitEngine はサブストリーム境界: 復号器を境界へ再初期化し、正準側を
// 締めて次を開始する。
func (s *hevcTeeSink) reinitEngine(bytePos int) bool {
	if !s.finishSub(bytePos) {
		return false
	}
	if bytePos*8 >= len(s.d.r.b)*8 {
		s.d.err = true
		return false
	}
	s.d.reinitAt(bytePos * 8)
	return !s.d.err
}

// --- rebuild シンク ---

type hevcRebuildSink struct {
	dec   *rangeDecoder
	m     *hevcSecModel
	w     *h264Writer
	ce    *cabacEncoder
	st    [hevcNumContexts]uint8
	saved [hevcNumContexts]uint8

	fixes []hevcSubFix
	sub   int
	out   []byte // 復元済みボディ(スライスデータ部分)
	bad   bool
}

func (s *hevcRebuildSink) decision(ctx int) int {
	ps := hevcStateP0(s.st[ctx])
	bin := hevcDecodeBitMix(s.dec, &s.m.ctx[ctx], ps)
	s.ce.encodeDecision(&s.st[ctx], bin)
	return bin
}

func (s *hevcRebuildSink) bypassCls(cls int) int {
	bin := s.dec.decodeBitM(&s.m.byp[cls])
	s.ce.encodeBypass(bin)
	return bin
}

func (s *hevcRebuildSink) bypass() int {
	bin := s.dec.decodeBitEq()
	s.ce.encodeBypass(bin)
	return bin
}

func (s *hevcRebuildSink) terminate() int {
	bin := s.dec.decodeBitM(&s.m.term)
	s.ce.encodeTerminate(bin)
	return bin
}

func (s *hevcRebuildSink) failed() bool { return s.bad }

func (s *hevcRebuildSink) intraPCM(nBytes int) bool {
	data := make([]byte, nBytes)
	for i := range data {
		v := 0
		for k := 0; k < 8; k++ {
			v = v<<1 | s.dec.decodeBitEq()
		}
		data[i] = byte(v)
	}
	s.ce.pcmInsert(data)
	return true
}

func (s *hevcRebuildSink) initCtx(sliceQP, initType int) {
	hevcInitStates(&s.st, sliceQP, initType)
}
func (s *hevcRebuildSink) saveCtx() { s.saved = s.st }
func (s *hevcRebuildSink) loadCtx() { s.st = s.saved }

// finishSub は現サブストリームの正準出力へ [delta][tail] を適用し out に足す。
func (s *hevcRebuildSink) finishSub() bool {
	if s.sub >= len(s.fixes) {
		s.bad = true
		return false
	}
	for s.w.nbit%8 != 0 {
		s.w.u1(0)
	}
	canon := s.w.b
	fix := s.fixes[s.sub]
	s.sub++
	match := len(canon) - fix.delta
	if match < 0 {
		s.bad = true
		return false
	}
	s.out = append(s.out, canon[:match]...)
	s.out = append(s.out, fix.tail...)
	s.w = &h264Writer{}
	s.ce = newCabacEncoder(s.w)
	return true
}

func (s *hevcRebuildSink) reinitEngine(bytePos int) bool {
	return s.finishSub()
}
