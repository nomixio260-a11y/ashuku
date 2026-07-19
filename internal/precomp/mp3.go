package precomp

// MP3 可逆再圧縮の最上層(capture / rebuild / コンテナ)。
//
// capture はグラニュールごとに2パスで安全に進める:
//  1. 記録パス: プールから記号列をイベントとして読み取る(失敗検知)。
//  2. 正準検証: イベントを固定ハフマンで再符号化し、原文ビットと完全一致
//     するか確認。一致したグラニュールだけ「圧縮フラグ=1」で算術へ、
//     一致しない(境界跨ぎ等)グラニュールは「フラグ=0」で原文ビットを
//     そのまま算術のバイパスに流す。→ 可逆性は常に成立。
// グラニュール間の隙間(アンシラリ)とフレーム末尾は生ビットで保存する。
// 位置関係はすべて骨格(サイド情報)から決定的に再計算できるため、
// レシピに位置情報は不要。

import "bytes"

// --- イベント ---

type mp3EvKind uint8

const (
	mp3EvSF mp3EvKind = iota
	mp3EvHuff
	mp3EvQuad
	mp3EvCont
	mp3EvRaw
)

type mp3Event struct {
	kind mp3EvKind
	a    int32 // sf: slen / huff: table / quad: tsel / raw: nbits
	b    int32 // 値
	c    int32 // 位置(sf: idx / huff,quad: pos)
}

// mp3RecSink は読み取りパスでイベントを記録する。
type mp3RecSink struct {
	evs []mp3Event
	bad bool
}

func (s *mp3RecSink) sf(slen, idx, v int) int {
	s.evs = append(s.evs, mp3Event{mp3EvSF, int32(slen), int32(v), int32(idx)})
	return v
}
func (s *mp3RecSink) huff(t, pos, sym int) int {
	s.evs = append(s.evs, mp3Event{mp3EvHuff, int32(t), int32(sym), int32(pos)})
	return sym
}
func (s *mp3RecSink) quad(tsel, pos, sym int) int {
	s.evs = append(s.evs, mp3Event{mp3EvQuad, int32(tsel), int32(sym), int32(pos)})
	return sym
}
func (s *mp3RecSink) cont(v int) int {
	s.evs = append(s.evs, mp3Event{mp3EvCont, 0, int32(v), 0})
	return v
}
func (s *mp3RecSink) raw(n int, v uint32) uint32 {
	s.evs = append(s.evs, mp3Event{mp3EvRaw, int32(n), int32(v), 0})
	return v
}
func (s *mp3RecSink) granule()     {}
func (s *mp3RecSink) fail()        { s.bad = true }
func (s *mp3RecSink) failed() bool { return s.bad }

// mp3ReplaySink は記録済みイベントを書き経路へ再生する。
type mp3ReplaySink struct {
	evs []mp3Event
	i   int
	bad bool
}

func (s *mp3ReplaySink) next(kind mp3EvKind) *mp3Event {
	if s.i >= len(s.evs) || s.evs[s.i].kind != kind {
		s.bad = true
		return nil
	}
	e := &s.evs[s.i]
	s.i++
	return e
}
func (s *mp3ReplaySink) sf(slen, idx, v int) int {
	if e := s.next(mp3EvSF); e != nil && int(e.a) == slen {
		return int(e.b)
	}
	s.bad = true
	return 0
}
func (s *mp3ReplaySink) huff(t, pos, sym int) int {
	if e := s.next(mp3EvHuff); e != nil && int(e.a) == t {
		return int(e.b)
	}
	s.bad = true
	return 0
}
func (s *mp3ReplaySink) quad(tsel, pos, sym int) int {
	if e := s.next(mp3EvQuad); e != nil && int(e.a) == tsel {
		return int(e.b)
	}
	s.bad = true
	return 0
}
func (s *mp3ReplaySink) cont(v int) int {
	if e := s.next(mp3EvCont); e != nil {
		return int(e.b)
	}
	return 0
}
func (s *mp3ReplaySink) raw(n int, v uint32) uint32 {
	if e := s.next(mp3EvRaw); e != nil && int(e.a) == n {
		return uint32(e.b)
	}
	s.bad = true
	return 0
}
func (s *mp3ReplaySink) granule()     {}
func (s *mp3ReplaySink) fail()        { s.bad = true }
func (s *mp3ReplaySink) failed() bool { return s.bad }

// --- 算術モデル ---
//
// 文脈設計(packMP3 の発想):
//  - (x,y) 記号をニブルに分離し、**テーブル横断で共有**する。エンコーダの
//    テーブル切替で統計が分断されるのを防ぐ。
//  - 文脈 = 係数位置バケット(周波数帯)+ 直前ペアの振幅クラス。
//  - y は同ペアの x のクラスを文脈にする(x,y は強く相関)。

type mp3Models struct {
	gflag cbModel
	sf    [16][6][15]cbModel // [slen][帯域バケット][ビット位置]
	nibX  [8][4][16]cbModel  // [位置バケット][直前クラス][木ノード]
	nibY  [8][4][16]cbModel  // [位置バケット][x クラス][木ノード]
	esc   [4][16]cbModel     // linbits 上位4ビット [位置バケット/2]
	quad  [2][4][16]cbModel  // [tsel][位置バケット]
	cont  [4]cbModel
	gap   cbModel
}

func newMP3Models() *mp3Models {
	m := &mp3Models{}
	init1 := func(p *cbModel) { *p = newCbModel() }
	init1(&m.gflag)
	for i := range m.sf {
		for j := range m.sf[i] {
			for k := range m.sf[i][j] {
				init1(&m.sf[i][j][k])
			}
		}
	}
	for i := 0; i < 8; i++ {
		for c := 0; c < 4; c++ {
			for n := 0; n < 16; n++ {
				init1(&m.nibX[i][c][n])
				init1(&m.nibY[i][c][n])
			}
		}
	}
	for i := range m.esc {
		for n := range m.esc[i] {
			init1(&m.esc[i][n])
		}
	}
	for i := range m.quad {
		for j := range m.quad[i] {
			for n := range m.quad[i][j] {
				init1(&m.quad[i][j][n])
			}
		}
	}
	for i := range m.cont {
		init1(&m.cont[i])
	}
	init1(&m.gap)
	return m
}

// mp3Cls は振幅のクラス(0 / 1 / 2-3 / 4+)。
func mp3Cls(m int) int {
	switch {
	case m == 0:
		return 0
	case m == 1:
		return 1
	case m <= 3:
		return 2
	default:
		return 3
	}
}

func mp3PosBucket(pos int) int {
	b := pos >> 6
	if b > 7 {
		b = 7
	}
	return b
}

// nibble 木の符号化/復号(4ビット、16ノード木)。
func encNib(enc *rangeEncoder, mdl *[16]cbModel, v int) {
	node := 1
	for b := 3; b >= 0; b-- {
		bit := (v >> uint(b)) & 1
		enc.encodeBitM(&mdl[node], bit)
		node = node<<1 | bit
		if node >= 16 {
			node -= 8 // 下位レベルはノード共有(8..15 を再利用)
		}
	}
}

func decNib(dec *rangeDecoder, mdl *[16]cbModel) int {
	node := 1
	out := 0
	for b := 3; b >= 0; b-- {
		bit := dec.decodeBitM(&mdl[node])
		out = out<<1 | bit
		node = node<<1 | bit
		if node >= 16 {
			node -= 8
		}
	}
	return out
}

// mp3ArithCapture は記録済みイベントを算術符号へ流す。
type mp3ArithCapture struct {
	enc     *rangeEncoder
	m       *mp3Models
	contN   int
	prevCls int
}

func (s *mp3ArithCapture) granule() { s.prevCls = 0; s.contN = 0 }

func (s *mp3ArithCapture) sf(slen, idx, v int) int {
	ib := idx >> 2
	if ib > 5 {
		ib = 5
	}
	for b := slen - 1; b >= 0; b-- {
		s.enc.encodeBitM(&s.m.sf[slen][ib][b], (v>>uint(b))&1)
	}
	return v
}
func (s *mp3ArithCapture) huff(t, pos, sym int) int {
	ys := mp3HuffTables[t].YSize
	x, y := sym/ys, sym%ys
	pb := mp3PosBucket(pos)
	encNib(s.enc, &s.m.nibX[pb][s.prevCls], x)
	encNib(s.enc, &s.m.nibY[pb][mp3Cls(x)], y)
	mx := x
	if y > mx {
		mx = y
	}
	s.prevCls = mp3Cls(mx)
	return sym
}
func (s *mp3ArithCapture) quad(tsel, pos, sym int) int {
	pb := (pos >> 7) & 3
	node := 1
	for b := 3; b >= 0; b-- {
		bit := (sym >> uint(b)) & 1
		s.enc.encodeBitM(&s.m.quad[tsel][pb][node], bit)
		node = node<<1 | bit
		if node >= 16 {
			node -= 8
		}
	}
	return sym
}
func (s *mp3ArithCapture) cont(v int) int {
	bucket := s.contN >> 5
	if bucket > 3 {
		bucket = 3
	}
	s.enc.encodeBitM(&s.m.cont[bucket], v)
	s.contN++
	return v
}
func (s *mp3ArithCapture) raw(n int, v uint32) uint32 {
	for b := n - 1; b >= 0; b-- {
		s.enc.encodeBitEq(int(v>>uint(b)) & 1)
	}
	return v
}
func (s *mp3ArithCapture) fail()        {}
func (s *mp3ArithCapture) failed() bool { return false }

// mp3ArithRebuild は算術符号から記号を復号する。
type mp3ArithRebuild struct {
	dec     *rangeDecoder
	m       *mp3Models
	contN   int
	prevCls int
}

func (s *mp3ArithRebuild) granule() { s.prevCls = 0; s.contN = 0 }

func (s *mp3ArithRebuild) sf(slen, idx, v int) int {
	ib := idx >> 2
	if ib > 5 {
		ib = 5
	}
	out := 0
	for b := slen - 1; b >= 0; b-- {
		out = out<<1 | s.dec.decodeBitM(&s.m.sf[slen][ib][b])
	}
	return out
}
func (s *mp3ArithRebuild) huff(t, pos, sym int) int {
	ys := mp3HuffTables[t].YSize
	pb := mp3PosBucket(pos)
	x := decNib(s.dec, &s.m.nibX[pb][s.prevCls])
	y := decNib(s.dec, &s.m.nibY[pb][mp3Cls(x)])
	mx := x
	if y > mx {
		mx = y
	}
	s.prevCls = mp3Cls(mx)
	return x*ys + y
}
func (s *mp3ArithRebuild) quad(tsel, pos, sym int) int {
	pb := (pos >> 7) & 3
	node := 1
	out := 0
	for b := 3; b >= 0; b-- {
		bit := s.dec.decodeBitM(&s.m.quad[tsel][pb][node])
		out = out<<1 | bit
		node = node<<1 | bit
		if node >= 16 {
			node -= 8
		}
	}
	return out
}
func (s *mp3ArithRebuild) cont(v int) int {
	bucket := s.contN >> 5
	if bucket > 3 {
		bucket = 3
	}
	s.contN++
	return s.dec.decodeBitM(&s.m.cont[bucket])
}
func (s *mp3ArithRebuild) raw(n int, v uint32) uint32 {
	out := uint32(0)
	for b := 0; b < n; b++ {
		out = out<<1 | uint32(s.dec.decodeBitEq())
	}
	return out
}
func (s *mp3ArithRebuild) fail()        {}
func (s *mp3ArithRebuild) failed() bool { return false }

// --- グラニュール位置の決定的な列挙 ---

// mp3GranulePos は1グラニュールのプール内ビット範囲。
type mp3GranulePos struct {
	frame  int
	g, ch  int
	start  int // ビット
	length int
	usable bool // プール範囲内で解析対象か
}

// mp3LayoutGranules は全グラニュールのプール内位置を側情報から計算する。
// 骨格(サイド情報)だけから決定的に再計算できる。
func mp3LayoutGranules(frames []*mp3Frame, poolBits int) []mp3GranulePos {
	var out []mp3GranulePos
	for fi, f := range frames {
		start := (f.poolBase - f.si.mainDataBegin) * 8
		usable := start >= 0
		for g := 0; g < f.si.nGranules; g++ {
			for ch := 0; ch < f.hdr.nChannels; ch++ {
				l := f.si.gr[g][ch].part23Len
				gp := mp3GranulePos{frame: fi, g: g, ch: ch, start: start, length: l}
				gp.usable = usable && start+l <= poolBits
				if !gp.usable {
					usable = false // 以降のグラニュールも位置が信用できない
				}
				out = append(out, gp)
				start += l
			}
		}
	}
	return out
}

// --- capture ---

// MP3Recipe は再構成レシピ。Mode 0=算術モデル、1=レイアウト分離のみ
// (プール原文。骨格の zstd 圧縮効果だけを取る安全モード)。
type MP3Recipe struct {
	Prefix  int `json:"p"`
	NFrames int `json:"f"`
	SkelN   int `json:"sn"`
	Mode    int `json:"m,omitempty"`
}

// MP3Unwrapped は分解結果。
type MP3Unwrapped struct {
	Chunked []byte
	Recipe  *MP3Recipe
}

// TryUnwrapMP3 は MP3 を骨格+算術ストリームへ分解する。
func TryUnwrapMP3(orig []byte, maxPlain int64) (*MP3Unwrapped, bool) {
	if maxPlain <= 0 || maxPlain > maxPlainTotal {
		maxPlain = maxPlainTotal
	}
	if int64(len(orig)) > maxPlain || len(orig) < 128 || !IsMP3(orig) {
		return nil, false
	}
	prefix, frames, pool, _, ok := mp3Walk(orig)
	if !ok || len(pool) == 0 {
		return nil, false
	}
	// 骨格 = prefix + 各フレームの(ヘッダ+CRC+サイド情報)+ suffix
	var skel []byte
	skel = append(skel, orig[:prefix]...)
	for _, f := range frames {
		skel = append(skel, orig[f.off:f.dataOff]...)
	}
	lastEnd := frames[len(frames)-1].off + frames[len(frames)-1].hdr.frameSize
	skel = append(skel, orig[lastEnd:]...)

	enc := newRangeEncoder()
	models := newMP3Models()
	cap := &mp3ArithCapture{enc: enc, m: models}
	pr := &h264Reader{b: pool}
	poolBits := len(pool) * 8
	layout := mp3LayoutGranules(frames, poolBits)

	emitGap := func(to int) bool {
		if pr.pos > to {
			return false // 位置の逆行 = サイド情報が矛盾 → 素通し
		}
		for pr.pos < to {
			b, err := pr.u1()
			if err != nil {
				return false
			}
			enc.encodeBitM(&models.gap, int(b))
		}
		return true
	}

	for _, gp := range layout {
		if !gp.usable || gp.length == 0 {
			continue // 位置不明・空グラニュールは隙間として流れる
		}
		if !emitGap(gp.start) {
			return nil, false
		}
		f := frames[gp.frame]
		end := gp.start + gp.length
		// パス1: 記録
		rec := &mp3RecSink{}
		src := &mp3BitSrc{r: &h264Reader{b: pool, pos: gp.start}, end: end}
		okScan := mp3ScanGranule(rec, src, f.hdr, f.si, gp.g, gp.ch)
		if okScan {
			// 残りスタッフィングを記録
			for src.r.pos < end {
				n := end - src.r.pos
				if n > 24 {
					n = 24
				}
				v, err := src.r.u(n)
				if err != nil {
					okScan = false
					break
				}
				rec.evs = append(rec.evs, mp3Event{mp3EvRaw, int32(n), int32(v), 0})
			}
		}
		// パス2: 正準検証
		if okScan {
			okScan = mp3VerifyGranule(rec.evs, pool, gp, f)
		}
		if okScan {
			enc.encodeBitM(&models.gflag, 1)
			replayToArith(rec.evs, cap)
		} else {
			enc.encodeBitM(&models.gflag, 0)
			// 原文ビットをバイパスで
			rr := &h264Reader{b: pool, pos: gp.start}
			for rr.pos < end {
				b, err := rr.u1()
				if err != nil {
					return nil, false
				}
				enc.encodeBitEq(int(b))
			}
		}
		pr.pos = end
	}
	if !emitGap(poolBits) {
		return nil, false
	}
	arith := enc.finish()

	// モード選択: 算術モデル vs レイアウト分離のみ(プール原文)。後段 zstd の
	// 見込みサイズで小さい方を採用する(骨格は必ず縮むので、モデルが不利な
	// 高密度音源でも素通しにはならない)。
	recipe := &MP3Recipe{Prefix: prefix, NFrames: len(frames), SkelN: len(skel)}
	modeled := append(append(make([]byte, 0, len(skel)+len(arith)), skel...), arith...)
	layoutOnly := append(append(make([]byte, 0, len(skel)+len(pool)), skel...), pool...)
	pm := len(jpegProbeEncoder.EncodeAll(modeled, nil))
	pl := len(jpegProbeEncoder.EncodeAll(layoutOnly, nil))
	chunked := modeled
	if pl < pm {
		recipe.Mode = 1
		chunked = layoutOnly
		pm = pl
	}
	// 素の zstd(非採用時も後段で適用される)より有利な場合だけ採用する。
	base := len(jpegProbeEncoder.EncodeAll(orig, nil))
	if pm+96 >= base {
		return nil, false
	}
	rt, err := ReconstructMP3(recipe, chunked)
	if err != nil || !bytes.Equal(rt, orig) {
		return nil, false
	}
	return &MP3Unwrapped{Chunked: chunked, Recipe: recipe}, true
}

// replayToArith は記録イベントを算術シンクへ流す。
func replayToArith(evs []mp3Event, cap *mp3ArithCapture) {
	cap.granule()
	for _, e := range evs {
		switch e.kind {
		case mp3EvSF:
			cap.sf(int(e.a), int(e.c), int(e.b))
		case mp3EvHuff:
			cap.huff(int(e.a), int(e.c), int(e.b))
		case mp3EvQuad:
			cap.quad(int(e.a), int(e.c), int(e.b))
		case mp3EvCont:
			cap.cont(int(e.b))
		case mp3EvRaw:
			cap.raw(int(e.a), uint32(e.b))
		}
	}
}

// mp3VerifyGranule はイベント列を正準再符号化し原文ビットと比較する。
func mp3VerifyGranule(evs []mp3Event, pool []byte, gp mp3GranulePos, f *mp3Frame) bool {
	rep := &mp3ReplaySink{evs: evs}
	w := &h264Writer{}
	src := &mp3BitSrc{w: w}
	if !mp3ScanGranule(rep, src, f.hdr, f.si, gp.g, gp.ch) || rep.bad {
		return false
	}
	// スタッフィング(残イベントは raw のはず)
	for rep.i < len(rep.evs) {
		e := rep.evs[rep.i]
		if e.kind != mp3EvRaw {
			return false
		}
		w.u(uint32(e.b), int(e.a))
		rep.i++
	}
	if w.nbit != gp.length {
		return false
	}
	// ビット比較
	rr := &h264Reader{b: pool, pos: gp.start}
	ww := &h264Reader{b: w.b}
	for i := 0; i < gp.length; i++ {
		a, err1 := rr.u1()
		b, err2 := ww.u1()
		if err1 != nil || err2 != nil || a != b {
			return false
		}
	}
	return true
}

// ReconstructMP3 はレシピと Chunked から元の MP3 をバイト単位で戻す。
func ReconstructMP3(recipe *MP3Recipe, chunked []byte) ([]byte, error) {
	if recipe == nil || recipe.SkelN < 0 || recipe.SkelN > len(chunked) ||
		recipe.Prefix < 0 || recipe.Prefix > recipe.SkelN || recipe.NFrames <= 0 {
		return nil, errMP3
	}
	skel := chunked[:recipe.SkelN]
	arith := chunked[recipe.SkelN:]
	_ = arith

	// 骨格からフレーム構造を復元
	type rframe struct {
		hdr     *mp3FrameHdr
		si      *mp3SideInfo
		skelOff int
		dataLen int
	}
	var rfs []*mp3Frame
	var skelOffs []int
	pos := recipe.Prefix
	for i := 0; i < recipe.NFrames; i++ {
		h := parseMP3Header(skel[pos:])
		if h == nil {
			return nil, errMP3
		}
		hdrCrc := 4
		if h.crc {
			hdrCrc = 6
		}
		if pos+hdrCrc+h.sideBytes > len(skel) {
			return nil, errMP3
		}
		sr := &h264Reader{b: skel[pos+hdrCrc:]}
		si, ok := parseMP3SideInfo(sr, h)
		if !ok {
			return nil, errMP3
		}
		f := &mp3Frame{off: pos, hdr: h, si: si,
			dataLen: h.frameSize - hdrCrc - h.sideBytes}
		if len(rfs) > 0 {
			p := rfs[len(rfs)-1]
			f.poolBase = p.poolBase + p.dataLen
		}
		rfs = append(rfs, f)
		skelOffs = append(skelOffs, pos)
		pos += hdrCrc + h.sideBytes
	}
	suffixOff := pos
	poolBytes := 0
	for _, f := range rfs {
		poolBytes += f.dataLen
	}
	poolBits := poolBytes * 8
	if recipe.Mode == 1 {
		if len(arith) < poolBytes {
			return nil, errMP3
		}
		return mp3Assemble(recipe, skel, skelOffs, rfs, suffixOff, arith[:poolBytes]), nil
	}

	// プールをビット単位で再生成
	dec := newRangeDecoder(arith)
	models := newMP3Models()
	reb := &mp3ArithRebuild{dec: dec, m: models}
	w := &h264Writer{}
	layout := mp3LayoutGranules(rfs, poolBits)

	fillGap := func(to int) error {
		if w.nbit > to {
			return errMP3
		}
		for w.nbit < to {
			w.u1(uint32(dec.decodeBitM(&models.gap)))
		}
		return nil
	}
	for _, gp := range layout {
		if !gp.usable || gp.length == 0 {
			continue
		}
		if err := fillGap(gp.start); err != nil {
			return nil, err
		}
		f := rfs[gp.frame]
		flag := dec.decodeBitM(&models.gflag)
		if flag == 1 {
			src := &mp3BitSrc{w: w}
			startBits := w.nbit
			if !mp3ScanGranule(reb, src, f.hdr, f.si, gp.g, gp.ch) {
				return nil, errMP3
			}
			// スタッフィング
			rem := gp.length - (w.nbit - startBits)
			if rem < 0 {
				return nil, errMP3
			}
			for rem > 0 {
				n := rem
				if n > 24 {
					n = 24
				}
				v := reb.raw(n, 0)
				w.u(v, n)
				rem -= n
			}
		} else {
			for i := 0; i < gp.length; i++ {
				w.u1(uint32(dec.decodeBitEq()))
			}
		}
	}
	if err := fillGap(poolBits); err != nil {
		return nil, err
	}
	pool := w.b
	if len(pool) < poolBytes {
		pool = append(pool, make([]byte, poolBytes-len(pool))...)
	}
	return mp3Assemble(recipe, skel, skelOffs, rfs, suffixOff, pool), nil
}

// mp3Assemble は骨格+プールからファイルを合成する。
func mp3Assemble(recipe *MP3Recipe, skel []byte, skelOffs []int, rfs []*mp3Frame, suffixOff int, pool []byte) []byte {
	var out []byte
	out = append(out, skel[:recipe.Prefix]...)
	pp := 0
	for i, f := range rfs {
		hdrCrc := 4
		if f.hdr.crc {
			hdrCrc = 6
		}
		out = append(out, skel[skelOffs[i]:skelOffs[i]+hdrCrc+f.hdr.sideBytes]...)
		out = append(out, pool[pp:pp+f.dataLen]...)
		pp += f.dataLen
	}
	out = append(out, skel[suffixOff:]...)
	return out
}
