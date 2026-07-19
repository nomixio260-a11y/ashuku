package precomp

// H.264 CABAC のスライス構文解析(ピクセル復号なし、I/P/B 対応)。
//
// CABAC は文脈選択が構文依存なので、可逆再圧縮にはビンごとの ctxIdx を
// 正確に再現する必要がある。ここでは FFmpeg(=規格)と同一の文脈導出で
// スライスデータを走査する。係数値・IDCT・動き予測は不要——文脈に要る
// 近傍状態(4x4 非ゼロ数、cbp16、skip/direct/intra/8x8DCT フラグ、
// ref>0、|mvd|)だけを追跡する。
//
// bin プリミティブ(decision/bypass/terminate)は cabacSink 抽象を通す:
//  - 検証: cabacDecoder で復号するだけ
//  - capture: 復号したビンを二次算術符号へ(+正準 CABAC 再符号化を並走)
//  - rebuild: 二次算術から復号→ CABAC へ再符号化
// 3経路とも同じ文脈状態を同じ順で更新するので lockstep が保たれる。

// cabacSink はビン入出力の抽象。decision は ctxIdx 付き、bypass/terminate は別枠。
// intraPCM は I_PCM MB(mbSize バイトの生画素)を処理し復号器を再初期化する。
type cabacSink interface {
	decision(ctxIdx int) int
	bypass() int
	terminate() int
	intraPCM(mbSizeBytes int) bool
	failed() bool
}

// cabacPCMAlign は I_PCM のバイト境界計算に使う「消費位置」からの調整
// (実ストリームで較正済み: 0)。C = cr.pos - 9 を基準に切り上げ整列する。
var cabacPCMAlign = 0

// --- 検証用シンク(復号のみ) ---
type cabacDecSink struct {
	d  *cabacDecoder
	st *[1024]uint8
}

func (s *cabacDecSink) decision(ctx int) int { return s.d.decodeDecision(&s.st[ctx]) }
func (s *cabacDecSink) bypass() int          { return s.d.decodeBypass() }
func (s *cabacDecSink) terminate() int       { return s.d.decodeTerminate() }
func (s *cabacDecSink) failed() bool         { return s.d.err }

// intraPCM: I_PCM 検出直後に呼ぶ。復号器の消費位置 C=pos-9 をバイト境界へ
// 切り上げ、pcm 生バイト mbSize を読み飛ばし、その後ろから再初期化する。
func (s *cabacDecSink) intraPCM(mbSize int) bool {
	c := s.d.r.pos - 9 + cabacPCMAlign
	pcmStart := (c + 7) &^ 7
	pcmEnd := pcmStart + mbSize*8
	if pcmEnd > len(s.d.r.b)*8 {
		s.d.err = true
		return false
	}
	s.d.reinitAt(pcmEnd)
	return !s.d.err
}

// cabacSliceCtx はスライス種別と走査に要るパラメータ。
type cabacSliceCtx struct {
	sliceType int // 0=P, 1=B, 2=I
	initIDC   int
	numRef    [2]int
	dct8x8    bool // pps.transform_8x8_mode
	direct8x8 bool // sps.direct_8x8_inference
}

func (sc *cabacSliceCtx) isI() bool { return sc.sliceType == 2 }
func (sc *cabacSliceCtx) isB() bool { return sc.sliceType == 1 }

// cabacMBState はスライス走査に要る近傍状態。
type cabacMBState struct {
	mbW, mbH       int
	sliceID        []int32
	curSlice       int32
	cbp16          []int32 // MB ごと(bits0-3 luma cbp, 4-5 chroma, 6-7 chromaDC, 8 lumaDC)
	mbFlags        []uint8 // mbfIntra/mbfSkip/mbfDirect16/mbf8x8DCT/mbfI16
	chromaPred     []int32 // chroma_pred_mode_table(inter は 0)
	subDirect      []uint8 // 8x8 ごとの direct(bit 0..3)
	refs           [2][]int8
	mvdAbs         [2][][2]uint8
	nz             *h264NZ // 4x4 ごとの非ゼロ数(luma/chroma AC)
	lastDqpNonzero bool
	curIntra       bool // 現在走査中 MB が intra(cbf/cbp 文脈の既定値用)
	cache          interCache
}

func newCabacMBState(mbW, mbH int) *cabacMBState {
	n := mbW * mbH
	s := &cabacMBState{mbW: mbW, mbH: mbH,
		sliceID:    make([]int32, n),
		cbp16:      make([]int32, n),
		mbFlags:    make([]uint8, n),
		chromaPred: make([]int32, n),
		subDirect:  make([]uint8, n),
		nz:         newH264NZ(mbW, mbH),
	}
	for l := 0; l < 2; l++ {
		s.refs[l] = make([]int8, n*4)
		s.mvdAbs[l] = make([][2]uint8, n*16)
	}
	for i := range s.sliceID {
		s.sliceID[i] = -1
	}
	return s
}

func (s *cabacMBState) avail(mb int) bool { return mb >= 0 && s.sliceID[mb] == s.curSlice }

func (s *cabacMBState) isI16(mb int) bool { return s.mbFlags[mb]&mbfI16 != 0 }

// topCbp/leftCbp は cbf/cbp 文脈用の近傍 cbp(非MBAFF・フレーム・4:2:0)。
// 利用不可の既定値は現在 MB が intra なら 0x7CF、inter なら 0x00F(FFmpeg
// fill_decode_caches と同一)。
func (s *cabacMBState) cbpDefault() int32 {
	if s.curIntra {
		return 0x7CF
	}
	return 0x00F
}

func (s *cabacMBState) topCbp(mb int) int32 {
	t := mb - s.mbW
	if s.avail(t) {
		return s.cbp16[t]
	}
	return s.cbpDefault()
}

func (s *cabacMBState) leftCbp(mb int) int32 {
	if mb%s.mbW == 0 {
		return s.cbpDefault()
	}
	l := mb - 1
	if !s.avail(l) {
		return s.cbpDefault()
	}
	L := s.cbp16[l]
	return (L & 0x7F0) | ((L >> 0) & 2) | (((L >> 2) & 2) << 2)
}

// nzDefault は cbf 文脈の「利用不可」既定値(intra=64 / inter=0)。
func (s *cabacMBState) nzDefault() int {
	if s.curIntra {
		return 64
	}
	return 0
}

// --- スライス走査 ---

// cabacISlice は I スライス互換ラッパ(既存テスト用)。
func cabacISlice(sink cabacSink, st *cabacMBState, firstMB, total, qpY int) bool {
	return cabacSlice(sink, st, &cabacSliceCtx{sliceType: 2}, firstMB, total, qpY)
}

// cabacSlice は1スライスのデータを走査する(sink 経由)。
// 成功で true。対象外構文や末尾不整合で false。
func cabacSlice(sink cabacSink, st *cabacMBState, sc *cabacSliceCtx, firstMB, total, qpY int) bool {
	st.curSlice++
	st.nz.curSlice = st.curSlice
	st.lastDqpNonzero = false // last_qscale_diff はスライス開始で 0
	curr := firstMB
	for {
		if curr >= total || sink.failed() {
			return false
		}
		st.sliceID[curr] = st.curSlice
		st.nz.sliceID[curr] = st.curSlice
		if !sc.isI() && cabacMBSkip(sink, st, curr, sc.isB()) != 0 {
			st.markSkipMB(curr, sc.isB())
		} else if !cabacMB(sink, st, sc, curr) {
			return false
		}
		curr++
		// end_of_slice_flag
		if sink.terminate() == 1 {
			break
		}
	}
	_ = qpY
	return !sink.failed() && curr <= total
}

// markSkipMB は P_SKIP / B_SKIP の状態更新(構文は読まない)。
func (st *cabacMBState) markSkipMB(mb int, isB bool) {
	st.mbFlags[mb] = mbfSkip
	if isB {
		st.mbFlags[mb] |= mbfDirect16
		st.subDirect[mb] = 0xF
	} else {
		st.subDirect[mb] = 0
	}
	st.cbp16[mb] = 0
	st.chromaPred[mb] = 0
	for l := 0; l < 2; l++ {
		v := int8(refNU)
		if !isB && l == 0 {
			v = 0 // P_SKIP は L0 ref0
		}
		for i8 := 0; i8 < 4; i8++ {
			st.refs[l][mb*4+i8] = v
		}
		for b := 0; b < 16; b++ {
			st.mvdAbs[l][mb*16+b] = [2]uint8{}
		}
	}
	for blk := 0; blk < 16; blk++ {
		st.nz.setLuma(mb, blk, 0)
	}
	for c := 0; c < 2; c++ {
		for blk := 0; blk < 4; blk++ {
			st.nz.setChroma(mb, c, blk, 0)
		}
	}
	st.lastDqpNonzero = false
}

// markIntraPredState は intra MB の inter 近傍状態(ref/mvd/direct)を設定する。
func (st *cabacMBState) markIntraPredState(mb int) {
	st.subDirect[mb] = 0
	for l := 0; l < 2; l++ {
		for i8 := 0; i8 < 4; i8++ {
			st.refs[l][mb*4+i8] = refNU
		}
		for b := 0; b < 16; b++ {
			st.mvdAbs[l][mb*16+b] = [2]uint8{}
		}
	}
}

// cabacMB は1マクロブロック(非スキップ)を走査する。
func cabacMB(sink cabacSink, st *cabacMBState, sc *cabacSliceCtx, mb int) bool {
	st.mbFlags[mb] = 0
	intraMBT := -1 // 0=I_NxN, 1..24=I16, 25=I_PCM
	shape := 0     // inter: 0=16x16,1=16x8,2=8x16,3=8x8(P)/ B: bMBTable idx
	var bInfo bMBInfo

	switch sc.sliceType {
	case 2: // I
		// mb_type(I スライス、ctx_base=3, intra_slice=1)
		ctx := 0
		if l := mb - 1; mb%st.mbW != 0 && st.avail(l) && st.isI16(l) {
			ctx++
		}
		if t := mb - st.mbW; st.avail(t) && st.isI16(t) {
			ctx++
		}
		if sink.decision(3+ctx) == 0 {
			intraMBT = 0
		} else if sink.terminate() == 1 {
			intraMBT = 25
		} else {
			mbt := 1
			mbt += 12 * sink.decision(6)
			if sink.decision(7) != 0 {
				mbt += 4 + 4*sink.decision(8)
			}
			mbt += 2 * sink.decision(9)
			mbt += 1 * sink.decision(10)
			intraMBT = mbt
		}
	case 0: // P
		var esc bool
		shape, esc = cabacPMBType(sink)
		if esc {
			intraMBT = cabacIntraMBTypePB(sink, 17)
		}
	default: // B
		var esc bool
		idx, esc := cabacBMBType(sink, st, mb)
		if esc {
			intraMBT = cabacIntraMBTypePB(sink, 32)
		} else {
			shape = idx
			bInfo = bMBTable[idx]
		}
	}

	if intraMBT >= 0 {
		return cabacIntraBody(sink, st, sc, mb, intraMBT)
	}
	return cabacInterBody(sink, st, sc, mb, shape, bInfo)
}

// cabacIntraBody は intra MB の mb_type 後(I_PCM/予測モード/cbp/dqp/残差)。
func cabacIntraBody(sink cabacSink, st *cabacMBState, sc *cabacSliceCtx, mb int, mbt int) bool {
	st.curIntra = true
	st.mbFlags[mb] |= mbfIntra
	st.markIntraPredState(mb)
	if mbt == 25 { // I_PCM
		const mbSize420 = 384
		if !sink.intraPCM(mbSize420) {
			return false
		}
		// FFmpeg: cbp_table=0xf7ef 相当(全 coded)、nz=16、chroma_pred=0。
		st.chromaPred[mb] = 0
		st.cbp16[mb] = 0x1EF
		for blk := 0; blk < 16; blk++ {
			st.nz.setLuma(mb, blk, 16)
		}
		for c := 0; c < 2; c++ {
			for blk := 0; blk < 4; blk++ {
				st.nz.setChroma(mb, c, blk, 16)
			}
		}
		st.lastDqpNonzero = false
		return true
	}
	i16 := mbt >= 1
	cbp := 0
	is8x8 := false
	if i16 {
		st.mbFlags[mb] |= mbfI16
		cbp = i16CBP(mbt)
	} else {
		// I_NxN: transform_size_8x8_flag(High)→ 予測モード 16(4x4)/4(8x8)
		if sc.dct8x8 {
			nts := 0
			if l := mb - 1; mb%st.mbW != 0 && st.avail(l) && st.mbFlags[l]&mbf8x8DCT != 0 {
				nts++
			}
			if t := mb - st.mbW; st.avail(t) && st.mbFlags[t]&mbf8x8DCT != 0 {
				nts++
			}
			if sink.decision(399+nts) != 0 {
				is8x8 = true
				st.mbFlags[mb] |= mbf8x8DCT
			}
		}
		n := 16
		if is8x8 {
			n = 4
		}
		for b := 0; b < n; b++ {
			if sink.decision(68) == 0 { // prev_intra_pred_mode_flag
				sink.decision(69)
				sink.decision(69)
				sink.decision(69)
			}
		}
	}
	// intra_chroma_pred_mode
	cctx := 0
	if l := mb - 1; mb%st.mbW != 0 && st.avail(l) && st.chromaPred[l] != 0 {
		cctx++
	}
	if t := mb - st.mbW; st.avail(t) && st.chromaPred[t] != 0 {
		cctx++
	}
	chromaPred := 0
	if sink.decision(64+cctx) != 0 {
		chromaPred = 1
		if sink.decision(64+3) != 0 {
			chromaPred = 2
			if sink.decision(64+3) != 0 {
				chromaPred = 3
			}
		}
	}
	st.chromaPred[mb] = int32(chromaPred)

	if !i16 {
		cbp = cabacCBP(sink, st, mb)
	}
	st.cbp16[mb] = int32(cbp & 0x3F)

	if cbp > 0 || i16 {
		cabacMBQPDelta(sink, st)
	} else {
		st.lastDqpNonzero = false
	}
	return cabacResidualMB(sink, st, mb, i16, is8x8, cbp)
}

// cabacInterBody は inter MB のパーティション層+cbp/変換サイズ/dqp/残差。
func cabacInterBody(sink cabacSink, st *cabacMBState, sc *cabacSliceCtx, mb int, shape int, bInfo bMBInfo) bool {
	st.curIntra = false
	st.subDirect[mb] = 0 // 前スライスの値が残らないようクリア
	isB := sc.isB()
	c := &st.cache
	st.fillInterCache(c, mb)
	nLists := 1
	if isB {
		nLists = 2
	}
	dct8x8Allowed := sc.dct8x8

	readRef := func(list, n int) int8 {
		if sc.numRef[list] > 1 {
			r := cabacRefIdx(sink, c, list, n, isB)
			if r < 0 || r >= sc.numRef[list] {
				return refNA // 壊れた入力 → 検証で弾く
			}
			return int8(r)
		}
		return 0
	}

	if !isB {
		// --- P ---
		switch shape {
		case 0: // 16x16
			r := readRef(0, 0)
			fillRectRef(&c.ref[0], cScan8[0], 4, 4, r)
			mvd := cabacMVD(sink, c, 0, 0)
			fillRectMvd(&c.mvd[0], cScan8[0], 4, 4, mvd)
		case 1: // 16x8
			for i := 0; i < 2; i++ {
				r := readRef(0, 8*i)
				fillRectRef(&c.ref[0], cScan8[0]+16*i, 4, 2, r)
			}
			for i := 0; i < 2; i++ {
				mvd := cabacMVD(sink, c, 0, 8*i)
				fillRectMvd(&c.mvd[0], cScan8[0]+16*i, 4, 2, mvd)
			}
		case 2: // 8x16
			for i := 0; i < 2; i++ {
				r := readRef(0, 4*i)
				fillRectRef(&c.ref[0], cScan8[0]+2*i, 2, 4, r)
			}
			for i := 0; i < 2; i++ {
				mvd := cabacMVD(sink, c, 0, 4*i)
				fillRectMvd(&c.mvd[0], cScan8[0]+2*i, 2, 4, mvd)
			}
		default: // P_8x8
			var sub [4]int
			for i := 0; i < 4; i++ {
				sub[i] = cabacPSubType(sink)
			}
			for i := 0; i < 4; i++ {
				r := readRef(0, 4*i)
				fillRectRef(&c.ref[0], cScan8[4*i], 2, 2, r)
			}
			for i := 0; i < 4; i++ {
				if sub[i] != 0 {
					dct8x8Allowed = false
				}
				if !cabacSubMVD(sink, c, 0, i, sub[i]) {
					return false
				}
			}
		}
	} else {
		// --- B ---
		if bInfo.shape == 0 { // B_Direct_16x16
			st.mbFlags[mb] |= mbfDirect16
			st.subDirect[mb] = 0xF
			for l := 0; l < 2; l++ {
				fillRectRef(&c.ref[l], cScan8[0], 4, 4, refNU)
				fillRectMvd(&c.mvd[l], cScan8[0], 4, 4, [2]uint8{})
			}
			fillRectDirect(&c.direct, cScan8[0], 4, 4, true)
			if !sc.direct8x8 {
				dct8x8Allowed = false
			}
		} else if bInfo.shape == 4 { // B_8x8
			var sub [4]bSubInfo
			for i := 0; i < 4; i++ {
				sub[i] = bSubTable[cabacBSubType(sink)]
			}
			for list := 0; list < 2; list++ {
				for i := 0; i < 4; i++ {
					if sub[i].shape == 0 {
						continue // direct
					}
					if sub[i].dir[list] {
						r := readRef(list, 4*i)
						fillRectRef(&c.ref[list], cScan8[4*i], 2, 2, r)
					} else {
						fillRectRef(&c.ref[list], cScan8[4*i], 2, 2, refNU)
					}
				}
			}
			for list := 0; list < 2; list++ {
				for i := 0; i < 4; i++ {
					if sub[i].shape == 0 {
						fillRectMvd(&c.mvd[list], cScan8[4*i], 2, 2, [2]uint8{})
						continue
					}
					if sub[i].dir[list] {
						if !cabacSubMVDB(sink, c, list, i, sub[i]) {
							return false
						}
					} else {
						fillRectMvd(&c.mvd[list], cScan8[4*i], 2, 2, [2]uint8{})
					}
				}
			}
			for i := 0; i < 4; i++ {
				if sub[i].shape == 0 {
					st.subDirect[mb] |= 1 << uint(i)
					fillRectDirect(&c.direct, cScan8[4*i], 2, 2, true)
					if !sc.direct8x8 {
						dct8x8Allowed = false
					}
				} else if sub[i].shape != 1 {
					dct8x8Allowed = false
				}
			}
		} else {
			// 16x16 / 16x8 / 8x16
			nPart := 1
			if bInfo.shape >= 2 {
				nPart = 2
			}
			pos := func(i int) (p, w, h, n int) {
				if bInfo.shape == 1 {
					return cScan8[0], 4, 4, 0
				}
				if bInfo.shape == 2 { // 16x8
					return cScan8[0] + 16*i, 4, 2, 8 * i
				}
				return cScan8[0] + 2*i, 2, 4, 4 * i // 8x16
			}
			for list := 0; list < 2; list++ {
				for i := 0; i < nPart; i++ {
					p, w, h, n := pos(i)
					if bInfo.dir[i][list] {
						r := readRef(list, n)
						fillRectRef(&c.ref[list], p, w, h, r)
					} else {
						fillRectRef(&c.ref[list], p, w, h, refNU)
					}
				}
			}
			for list := 0; list < 2; list++ {
				for i := 0; i < nPart; i++ {
					p, w, h, n := pos(i)
					if bInfo.dir[i][list] {
						mvd := cabacMVD(sink, c, list, n)
						fillRectMvd(&c.mvd[list], p, w, h, mvd)
					} else {
						fillRectMvd(&c.mvd[list], p, w, h, [2]uint8{})
					}
				}
			}
		}
		_ = nLists
	}
	st.writeBackInter(c, mb)
	st.chromaPred[mb] = 0

	// cbp
	cbp := cabacCBP(sink, st, mb)
	st.cbp16[mb] = int32(cbp & 0x3F)

	// transform_size_8x8_flag(inter、cbp luma があるときのみ)
	is8x8 := false
	if dct8x8Allowed && cbp&15 != 0 {
		nts := 0
		if l := mb - 1; mb%st.mbW != 0 && st.avail(l) && st.mbFlags[l]&mbf8x8DCT != 0 {
			nts++
		}
		if t := mb - st.mbW; st.avail(t) && st.mbFlags[t]&mbf8x8DCT != 0 {
			nts++
		}
		if sink.decision(399+nts) != 0 {
			is8x8 = true
			st.mbFlags[mb] |= mbf8x8DCT
		}
	}

	if cbp > 0 {
		cabacMBQPDelta(sink, st)
	} else {
		st.lastDqpNonzero = false
	}
	return cabacResidualMB(sink, st, mb, false, is8x8, cbp)
}

// cabacPSubType は P の sub_mb_type(ctx 21..23)。0=8x8,1=8x4,2=4x8,3=4x4。
func cabacPSubType(sink cabacSink) int {
	if sink.decision(21) != 0 {
		return 0
	}
	if sink.decision(22) == 0 {
		return 1
	}
	if sink.decision(23) != 0 {
		return 2
	}
	return 3
}

// cabacBSubType は B の sub_mb_type(ctx 36..39)。bSubTable のインデックス。
func cabacBSubType(sink cabacSink) int {
	if sink.decision(36) == 0 {
		return 0
	}
	if sink.decision(37) == 0 {
		return 1 + sink.decision(39)
	}
	typ := 3
	if sink.decision(38) != 0 {
		if sink.decision(39) != 0 {
			return 11 + sink.decision(39)
		}
		typ += 4
	}
	typ += 2 * sink.decision(39)
	typ += sink.decision(39)
	return typ
}

// cabacSubMVD は P_8x8 の1個の 8x8 のサブパーティション mvd 群。
func cabacSubMVD(sink cabacSink, c *interCache, list, i8, subType int) bool {
	switch subType {
	case 0: // 8x8
		mvd := cabacMVD(sink, c, list, 4*i8)
		fillRectMvd(&c.mvd[list], cScan8[4*i8], 2, 2, mvd)
	case 1: // 8x4
		for j := 0; j < 2; j++ {
			n := 4*i8 + 2*j
			mvd := cabacMVD(sink, c, list, n)
			fillRectMvd(&c.mvd[list], cScan8[n], 2, 1, mvd)
		}
	case 2: // 4x8
		for j := 0; j < 2; j++ {
			n := 4*i8 + j
			mvd := cabacMVD(sink, c, list, n)
			fillRectMvd(&c.mvd[list], cScan8[n], 1, 2, mvd)
		}
	default: // 4x4
		for j := 0; j < 4; j++ {
			n := 4*i8 + j
			mvd := cabacMVD(sink, c, list, n)
			c.mvd[list][cScan8[n]] = mvd
		}
	}
	return true
}

// cabacSubMVDB は B_8x8 の1個の 8x8 のサブパーティション mvd 群。
func cabacSubMVDB(sink cabacSink, c *interCache, list, i8 int, sub bSubInfo) bool {
	switch sub.shape {
	case 1: // 8x8
		return cabacSubMVD(sink, c, list, i8, 0)
	case 2: // 8x4
		return cabacSubMVD(sink, c, list, i8, 1)
	case 3: // 4x8
		return cabacSubMVD(sink, c, list, i8, 2)
	default: // 4x4
		return cabacSubMVD(sink, c, list, i8, 3)
	}
}

// cabacCBP は coded_block_pattern(luma 4 + chroma)。
func cabacCBP(sink cabacSink, st *cabacMBState, mb int) int {
	la := st.leftCbp(mb)
	tb := st.topCbp(mb)
	cbp := 0
	c := b2int(la&0x02 == 0) + 2*b2int(tb&0x04 == 0)
	cbp += sink.decision(73 + c)
	c = b2int(int32(cbp)&0x01 == 0) + 2*b2int(tb&0x08 == 0)
	cbp += sink.decision(73+c) << 1
	c = b2int(la&0x08 == 0) + 2*b2int(int32(cbp)&0x01 == 0)
	cbp += sink.decision(73+c) << 2
	c = b2int(int32(cbp)&0x04 == 0) + 2*b2int(int32(cbp)&0x02 == 0)
	cbp += sink.decision(73+c) << 3
	// chroma
	ca := (la >> 4) & 3
	cb := (tb >> 4) & 3
	cc := 0
	if ca > 0 {
		cc++
	}
	if cb > 0 {
		cc += 2
	}
	if sink.decision(77+cc) != 0 {
		cc = 4
		if ca == 2 {
			cc++
		}
		if cb == 2 {
			cc += 2
		}
		cbp += (1 + sink.decision(77+cc)) << 4
	}
	return cbp
}

// cabacMBQPDelta は mb_qp_delta(ctx 60..63、9.3.3.1.1.5)。
func cabacMBQPDelta(sink cabacSink, st *cabacMBState) {
	ctx := 0
	if st.lastDqpNonzero {
		ctx = 1
	}
	if sink.decision(60+ctx) == 0 {
		st.lastDqpNonzero = false
		return
	}
	st.lastDqpNonzero = true
	// unary: state[62] 初回, 以降 state[63]
	if sink.decision(62) != 0 {
		i := 0
		for sink.decision(63) != 0 {
			i++
			if i > 128 {
				break
			}
		}
	}
}

func b2int(b bool) int {
	if b {
		return 1
	}
	return 0
}
