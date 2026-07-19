package precomp

// CABAC 可逆再圧縮のシンク。
//
// CABAC の文脈適応二値算術符号は 2003 年設計の 64 状態 FSM で、確率適応が
// 粗く、かつ**スライスごとに文脈を初期化**する。ここではバイト厳密に取り出した
// ビン列(cabacISlice が駆動)を、既存の二値レンジコーダ(rangecoder.go、
// LZMA 型・適応確率)へ**スライス跨ぎに持続**するモデルで載せ替える。
// 文脈コード化ビンでの推定改善と初期化オーバヘッドの回収が圧縮源。
// バイパスビン(等確率)は encodeBitEq で素通し。
//
// capture: 原 CABAC を復号しつつ各ビンを二次符号へ(ctxIdx をキーに)。
// rebuild: 二次符号から復号→ CABAC へ再符号化(原バイトを厳密再生)。
// 両経路とも cabacISlice が同じ ctxIdx 列を同順に生成するので lockstep。

// cbModel は速い/遅い2レートの適応カウンタ混合(各 11bit、和 12bit)。
// 単一レート(moveBits=5)より推定が滑らかで、CABAC ビン列で実測 +0.3〜0.5%
// 縮む。CABAC 経路専用(既存コーデックの stored 形式には影響しない)。
type cbModel struct{ fast, slow uint16 }

func newCbModel() cbModel { return cbModel{1024, 1024} }

func (m *cbModel) prob() uint32 { return uint32(m.fast) + uint32(m.slow) }
func (m *cbModel) update(bit int) {
	if bit == 0 {
		m.fast += (2048 - m.fast) >> 4
		m.slow += (2048 - m.slow) >> 7
	} else {
		m.fast -= m.fast >> 4
		m.slow -= m.slow >> 7
	}
}

// encodeBitM / decodeBitM は 12bit 混合確率での1ビン符号化(rangecoder.go の
// 11bit encodeBit と同じレンジ機構、確率の分解能だけ 1bit 高い)。
func (e *rangeEncoder) encodeBitM(m *cbModel, bit int) {
	bound := (e.rng >> 12) * m.prob()
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

func (d *rangeDecoder) decodeBitM(m *cbModel) int {
	bound := (d.rng >> 12) * m.prob()
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

// cabacSecModel は全 CABAC 文脈 + terminate の二次確率(スライス跨ぎ持続)。
type cabacSecModel struct {
	ctx  []cbModel
	term cbModel
}

func newCabacSecModel() *cabacSecModel {
	m := &cabacSecModel{ctx: make([]cbModel, 1024), term: newCbModel()}
	for i := range m.ctx {
		m.ctx[i] = newCbModel()
	}
	return m
}

// --- capture シンク(原CABAC→二次符号) ---

type cabacCaptureSink struct {
	d   *cabacDecoder
	st  *[1024]uint8
	enc *rangeEncoder
	m   *cabacSecModel
}

func (s *cabacCaptureSink) decision(ctx int) int {
	bin := s.d.decodeDecision(&s.st[ctx])
	s.enc.encodeBitM(&s.m.ctx[ctx], bin)
	return bin
}
func (s *cabacCaptureSink) bypass() int {
	bin := s.d.decodeBypass()
	s.enc.encodeBitEq(bin)
	return bin
}
func (s *cabacCaptureSink) terminate() int {
	bin := s.d.decodeTerminate()
	s.enc.encodeBitM(&s.m.term, bin)
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
		s.enc.encodeBitEq(v)
	}
	s.d.reinitAt(pcmEnd)
	return !s.d.err
}

// --- tee シンク(capture: 原CABAC復号 + 二次符号化 + 正準CABAC再符号化) ---
//
// 正準再符号化を capture 時に並走させるのは、rebuild が生成するバイト列
// (=正準)と原文の共通接頭辞長を求め、末尾のフラッシュ差(エンコーダ依存、
// 通常1バイト)を tail としてレシピに保存するため。これで任意エンコーダの
// 出力を完全可逆にできる。

type cabacTeeSink struct {
	d   *cabacDecoder
	stD *[1024]uint8
	rc  *rangeEncoder
	m   *cabacSecModel
	ce  *cabacEncoder
	stE *[1024]uint8
}

func (s *cabacTeeSink) decision(ctx int) int {
	bin := s.d.decodeDecision(&s.stD[ctx])
	s.rc.encodeBitM(&s.m.ctx[ctx], bin)
	s.ce.encodeDecision(&s.stE[ctx], bin)
	return bin
}
func (s *cabacTeeSink) bypass() int {
	bin := s.d.decodeBypass()
	s.rc.encodeBitEq(bin)
	s.ce.encodeBypass(bin)
	return bin
}
func (s *cabacTeeSink) terminate() int {
	bin := s.d.decodeTerminate()
	s.rc.encodeBitM(&s.m.term, bin)
	s.ce.encodeTerminate(bin)
	return bin
}
func (s *cabacTeeSink) failed() bool { return s.d.err }

// intraPCM: 生画素バイトを二次符号へ素通しし、復号器・正準符号化器の両方を
// バイト整列→再初期化する(terminate(1) のフラッシュは既に済んでいる)。
func (s *cabacTeeSink) intraPCM(mbSize int) bool {
	c := s.d.r.pos - 9 + cabacPCMAlign
	pcmStart := (c + 7) &^ 7
	pcmEnd := pcmStart + mbSize*8
	if pcmEnd > len(s.d.r.b)*8 || pcmStart%8 != 0 {
		s.d.err = true
		return false
	}
	data := s.d.r.b[pcmStart/8 : pcmEnd/8]
	for _, b := range data {
		for k := 7; k >= 0; k-- {
			s.rc.encodeBitEq(int(b>>uint(k)) & 1)
		}
	}
	s.d.reinitAt(pcmEnd)
	s.ce.pcmInsert(data)
	return !s.d.err
}

// --- rebuild シンク(二次符号→CABAC 再符号化) ---

type cabacRebuildSink struct {
	dec *rangeDecoder
	enc *cabacEncoder
	st  *[1024]uint8
	m   *cabacSecModel
}

func (s *cabacRebuildSink) decision(ctx int) int {
	bin := s.dec.decodeBitM(&s.m.ctx[ctx])
	s.enc.encodeDecision(&s.st[ctx], bin)
	return bin
}
func (s *cabacRebuildSink) bypass() int {
	bin := s.dec.decodeBitEq()
	s.enc.encodeBypass(bin)
	return bin
}
func (s *cabacRebuildSink) terminate() int {
	bin := s.dec.decodeBitM(&s.m.term)
	s.enc.encodeTerminate(bin)
	return bin
}
func (s *cabacRebuildSink) failed() bool { return false }

// intraPCM: 二次符号から生画素バイトを復元し、正準符号化器へ挿入する。
func (s *cabacRebuildSink) intraPCM(mbSize int) bool {
	data := make([]byte, mbSize)
	for i := range data {
		v := 0
		for k := 0; k < 8; k++ {
			v = v<<1 | s.dec.decodeBitEq()
		}
		data[i] = byte(v)
	}
	s.enc.pcmInsert(data)
	return true
}
