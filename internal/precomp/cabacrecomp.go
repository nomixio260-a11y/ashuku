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

// cabacSecModel は全 CABAC 文脈 + terminate の二次確率(スライス跨ぎ持続)。
type cabacSecModel struct {
	ctx  []bitModel
	term bitModel
}

func newCabacSecModel() *cabacSecModel {
	return &cabacSecModel{ctx: newModels(1024), term: modelInit}
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
	s.enc.encodeBit(&s.m.ctx[ctx], bin)
	return bin
}
func (s *cabacCaptureSink) bypass() int {
	bin := s.d.decodeBypass()
	s.enc.encodeBitEq(bin)
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
		s.enc.encodeBitEq(v)
	}
	s.d.reinitAt(pcmEnd)
	return !s.d.err
}

// --- rebuild シンク(二次符号→CABAC 再符号化) ---

type cabacRebuildSink struct {
	dec    *rangeDecoder
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
	bin := s.dec.decodeBitEq()
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
