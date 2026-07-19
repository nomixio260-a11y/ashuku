package precomp

// H.264 CABAC の P/B スライス inter 予測層(ピクセル復号なし)。
//
// 文脈導出に必要な近傍状態だけを追跡する:
//  - mb_skip_flag: 近傍の「利用可能かつ非スキップ」
//  - mb_type(B): 近傍の「利用可能かつ非ダイレクト」
//  - ref_idx: 近傍 8x8 の ref>0(B はダイレクト除外)
//  - mvd: 近傍 4x4 の |mvd| 成分和(70 でクリップした値の保存)
//  - transform_size_8x8_flag: 近傍 MB の 8x8DCT
// 動きベクトル値そのもの・予測(pred_motion)・direct 予測は構文に影響
// しないため一切計算しない。

// cScan8 は FFmpeg の scan8 と同じ 4x4 ブロック→キャッシュ位置(ストライド8、
// 行0=上境界、列3=左境界)。
var cScan8 = [16]int{
	4 + 1*8, 5 + 1*8, 4 + 2*8, 5 + 2*8,
	6 + 1*8, 7 + 1*8, 6 + 2*8, 7 + 2*8,
	4 + 3*8, 5 + 3*8, 4 + 4*8, 5 + 4*8,
	6 + 3*8, 7 + 3*8, 6 + 4*8, 7 + 4*8,
}

const (
	refNA = -2 // 枠外・スライス外
	refNU = -1 // このリスト不使用(intra 含む)
)

// mbFlag ビット
const (
	mbfIntra = 1 << iota
	mbfSkip
	mbfDirect16 // B_SKIP / B_Direct_16x16
	mbf8x8DCT
	mbfI16
)

// interCache は1MB 分の近傍込みキャッシュ(FFmpeg の *_cache 相当)。
type interCache struct {
	ref    [2][40]int8
	mvd    [2][40][2]uint8
	direct [40]bool
}

// fillInterCache は近傍 MB の保存値からキャッシュ境界を組み立てる。
func (st *cabacMBState) fillInterCache(c *interCache, mb int) {
	for l := 0; l < 2; l++ {
		for i := range c.ref[l] {
			c.ref[l][i] = refNA
			c.mvd[l][i] = [2]uint8{}
		}
	}
	for i := range c.direct {
		c.direct[i] = false
	}
	// 上隣接(下端行: 4x4 raster 12..15、8x8 ブロック 2,3)
	if t := mb - st.mbW; st.avail(t) {
		for i := 0; i < 4; i++ {
			cell := cScan8[0] - 8 + i
			for l := 0; l < 2; l++ {
				c.ref[l][cell] = st.refs[l][t*4+2+i/2]
				c.mvd[l][cell] = st.mvdAbs[l][t*16+12+i]
			}
			c.direct[cell] = st.subDirect[t]&(1<<(2+i/2)) != 0
		}
	}
	// 左隣接(右端列: 4x4 raster 3,7,11,15、8x8 ブロック 1,3)
	if l := mb - 1; mb%st.mbW != 0 && st.avail(l) {
		for j := 0; j < 4; j++ {
			cell := cScan8[0] - 1 + 8*j
			for li := 0; li < 2; li++ {
				c.ref[li][cell] = st.refs[li][l*4+1+2*(j/2)]
				c.mvd[li][cell] = st.mvdAbs[li][l*16+3+4*j]
			}
			c.direct[cell] = st.subDirect[l]&(1<<(1+2*(j/2))) != 0
		}
	}
}

// writeBackInter はキャッシュから per-MB 保存へ書き戻す。
// 保存は 4x4 ラスタ順(y*4+x)。キャッシュはラスタ配置なので cScan8[0] 起点で
// 直接走査する(cScan8[b] は Z 順なので使わない)。
func (st *cabacMBState) writeBackInter(c *interCache, mb int) {
	for l := 0; l < 2; l++ {
		for i8 := 0; i8 < 4; i8++ {
			st.refs[l][mb*4+i8] = c.ref[l][cScan8[4*i8]]
		}
		for y := 0; y < 4; y++ {
			for x := 0; x < 4; x++ {
				st.mvdAbs[l][mb*16+y*4+x] = c.mvd[l][cScan8[0]+8*y+x]
			}
		}
	}
}

// fillRect はキャッシュ矩形(w×h セル)を値で埋める。
func fillRectRef(a *[40]int8, pos, w, h int, v int8) {
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			a[pos+8*y+x] = v
		}
	}
}

func fillRectMvd(a *[40][2]uint8, pos, w, h int, v [2]uint8) {
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			a[pos+8*y+x] = v
		}
	}
}

func fillRectDirect(a *[40]bool, pos, w, h int, v bool) {
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			a[pos+8*y+x] = v
		}
	}
}

// cabacMBSkip は mb_skip_flag(ctx 11..13 / B: 24..26)。
func cabacMBSkip(sink cabacSink, st *cabacMBState, mb int, isB bool) int {
	ctx := 0
	if l := mb - 1; mb%st.mbW != 0 && st.avail(l) && st.mbFlags[l]&mbfSkip == 0 {
		ctx++
	}
	if t := mb - st.mbW; st.avail(t) && st.mbFlags[t]&mbfSkip == 0 {
		ctx++
	}
	if isB {
		ctx += 13
	}
	return sink.decision(11 + ctx)
}

// cabacRefIdx は ref_idx(ctx 54..59)。
func cabacRefIdx(sink cabacSink, c *interCache, list, n int, isB bool) int {
	refa := c.ref[list][cScan8[n]-1]
	refb := c.ref[list][cScan8[n]-8]
	ctx := 0
	if isB {
		if refa > 0 && !c.direct[cScan8[n]-1] {
			ctx++
		}
		if refb > 0 && !c.direct[cScan8[n]-8] {
			ctx += 2
		}
	} else {
		if refa > 0 {
			ctx++
		}
		if refb > 0 {
			ctx += 2
		}
	}
	ref := 0
	for sink.decision(54+ctx) != 0 {
		ref++
		ctx = (ctx >> 2) + 4
		if ref >= 32 {
			return -1
		}
	}
	return ref
}

// cabacMVDComp は1成分の mvd(ctxbase 40=x / 47=y)。|mvd| を 70 クリップで返す。
func cabacMVDComp(sink cabacSink, ctxbase, amvd int) int {
	inc := 0
	if amvd > 2 {
		inc++
	}
	if amvd > 32 {
		inc++
	}
	if sink.decision(ctxbase+inc) == 0 {
		return 0
	}
	mvd := 1
	cb := ctxbase + 3
	for mvd < 9 && sink.decision(cb) != 0 {
		if mvd < 4 {
			cb++
		}
		mvd++
	}
	if mvd >= 9 {
		k := 3
		for sink.bypass() != 0 {
			mvd += 1 << uint(k)
			k++
			if k > 24 {
				return -1 << 30 // 壊れた入力
			}
		}
		for k > 0 {
			k--
			mvd += sink.bypass() << uint(k)
		}
	}
	sink.bypass() // 符号
	if mvd > 70 {
		return 70
	}
	return mvd
}

// cabacMVD は (x,y) の mvd を読み、クリップ済み絶対値を返す。
func cabacMVD(sink cabacSink, c *interCache, list, n int) [2]uint8 {
	s8 := cScan8[n]
	ax := int(c.mvd[list][s8-1][0]) + int(c.mvd[list][s8-8][0])
	ay := int(c.mvd[list][s8-1][1]) + int(c.mvd[list][s8-8][1])
	mx := cabacMVDComp(sink, 40, ax)
	my := cabacMVDComp(sink, 47, ay)
	if mx < 0 || my < 0 {
		return [2]uint8{255, 255} // 検証で弾かれる想定
	}
	return [2]uint8{uint8(mx), uint8(my)}
}

// bMBInfo は B mb_type インデックス(0..22)の形状とリスト使用。
type bMBInfo struct {
	shape int // 0=direct16, 1=16x16, 2=16x8, 3=8x16, 4=8x8(sub)
	dir   [2][2]bool
}

var bMBTable = [23]bMBInfo{
	{0, [2][2]bool{}},
	{1, [2][2]bool{{true, false}}},
	{1, [2][2]bool{{false, true}}},
	{1, [2][2]bool{{true, true}}},
	{2, [2][2]bool{{true, false}, {true, false}}},
	{3, [2][2]bool{{true, false}, {true, false}}},
	{2, [2][2]bool{{false, true}, {false, true}}},
	{3, [2][2]bool{{false, true}, {false, true}}},
	{2, [2][2]bool{{true, false}, {false, true}}},
	{3, [2][2]bool{{true, false}, {false, true}}},
	{2, [2][2]bool{{false, true}, {true, false}}},
	{3, [2][2]bool{{false, true}, {true, false}}},
	{2, [2][2]bool{{true, false}, {true, true}}},
	{3, [2][2]bool{{true, false}, {true, true}}},
	{2, [2][2]bool{{false, true}, {true, true}}},
	{3, [2][2]bool{{false, true}, {true, true}}},
	{2, [2][2]bool{{true, true}, {true, false}}},
	{3, [2][2]bool{{true, true}, {true, false}}},
	{2, [2][2]bool{{true, true}, {false, true}}},
	{3, [2][2]bool{{true, true}, {false, true}}},
	{2, [2][2]bool{{true, true}, {true, true}}},
	{3, [2][2]bool{{true, true}, {true, true}}},
	{4, [2][2]bool{}},
}

// bSubInfo は B sub_mb_type(0..12)。shape: 0=direct, 1=8x8, 2=8x4, 3=4x8, 4=4x4。
type bSubInfo struct {
	shape int
	parts int
	dir   [2]bool
}

var bSubTable = [13]bSubInfo{
	{0, 1, [2]bool{}},
	{1, 1, [2]bool{true, false}},
	{1, 1, [2]bool{false, true}},
	{1, 1, [2]bool{true, true}},
	{2, 2, [2]bool{true, false}},
	{3, 2, [2]bool{true, false}},
	{2, 2, [2]bool{false, true}},
	{3, 2, [2]bool{false, true}},
	{2, 2, [2]bool{true, true}},
	{3, 2, [2]bool{true, true}},
	{4, 4, [2]bool{true, false}},
	{4, 4, [2]bool{false, true}},
	{4, 4, [2]bool{true, true}},
}

// cabacBMBType は B の mb_type ビンツリー(ctx 27..32、intra は escape)。
// 戻り: (bMBTable インデックス, intra=-1 のとき escape)。
func cabacBMBType(sink cabacSink, st *cabacMBState, mb int) (int, bool) {
	ctx := 0
	if l := mb - 1; mb%st.mbW != 0 && st.avail(l) && st.mbFlags[l]&mbfDirect16 == 0 {
		ctx++
	}
	if t := mb - st.mbW; st.avail(t) && st.mbFlags[t]&mbfDirect16 == 0 {
		ctx++
	}
	if sink.decision(27+ctx) == 0 {
		return 0, false // B_Direct_16x16
	}
	if sink.decision(27+3) == 0 {
		return 1 + sink.decision(27+5), false // B_L0_16x16 / B_L1_16x16
	}
	bits := sink.decision(27+4) << 3
	bits += sink.decision(27+5) << 2
	bits += sink.decision(27+5) << 1
	bits += sink.decision(27 + 5)
	switch {
	case bits < 8:
		return bits + 3, false
	case bits == 13:
		return 0, true // intra escape
	case bits == 14:
		return 11, false
	case bits == 15:
		return 22, false
	default:
		bits = bits<<1 + sink.decision(27+5)
		return bits - 4, false
	}
}

// cabacPMBType は P の mb_type(ctx 14..17)。戻り: (shapeIdx 0..3, intraEscape)。
// shapeIdx: 0=16x16, 1=16x8, 2=8x16, 3=8x8。
func cabacPMBType(sink cabacSink) (int, bool) {
	if sink.decision(14) != 0 {
		return 0, true // intra escape
	}
	if sink.decision(15) == 0 {
		return 3 * sink.decision(16), false // 0:16x16 / 3:8x8
	}
	return 2 - sink.decision(17), false // 1:16x8 / 2:8x16
}

// cabacIntraMBType は intra_mb_type(P/B の escape 用、intra_slice=0)。
// ctxBase=17(P)/32(B)。戻り値: 0=I_NxN, 1..24=I_16x16, 25=I_PCM。
func cabacIntraMBTypePB(sink cabacSink, ctxBase int) int {
	if sink.decision(ctxBase) == 0 {
		return 0
	}
	if sink.terminate() == 1 {
		return 25
	}
	mbt := 1
	mbt += 12 * sink.decision(ctxBase+1)
	if sink.decision(ctxBase+2) != 0 {
		mbt += 4 + 4*sink.decision(ctxBase+2)
	}
	mbt += 2 * sink.decision(ctxBase+3)
	mbt += 1 * sink.decision(ctxBase+3)
	return mbt
}
