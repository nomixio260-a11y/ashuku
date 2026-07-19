package precomp

// H.264 CABAC の I スライス構文解析(ピクセル復号なし)。
//
// CABAC は文脈選択が構文依存なので、可逆再圧縮にはビンごとの ctxIdx を
// 正確に再現する必要がある。ここでは I スライス(mb_type-I / イントラ予測 /
// cbp / mb_qp_delta / 残差)を FFmpeg(= 規格)と同一の文脈導出で走査する。
// 係数値・qmul・IDCT は不要——cbf/有意マップ文脈に要る「4x4 ごとの非ゼロ数」
// と「MB ごとの cbp16」だけを近傍追跡する(CAVLC 実装と同じ発想)。
//
// bin プリミティブ(decision/bypass/terminate)は cabacSink 抽象を通す:
//  - 検証: cabacDecoder で復号するだけ
//  - capture: 復号したビンを自前レンジ符号へ(ctxIdx をキーに)
//  - rebuild: 自前レンジ符号から復号→ CABAC へ再符号化
// 3経路とも同じ ctxState(64状態)を同じ順で更新するので lockstep が保たれる。

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
// (実ストリームで較正)。C = cr.pos - 9 を基準に切り上げ整列する。
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
	pcmStart := (c + 7) &^ 7 // バイト境界へ切り上げ
	pcmEnd := pcmStart + mbSize*8
	if pcmEnd > len(s.d.r.b)*8 {
		s.d.err = true
		return false
	}
	s.d.reinitAt(pcmEnd)
	return !s.d.err
}

// cabacMBState は I スライス走査に要る近傍状態。
type cabacMBState struct {
	mbW, mbH       int
	sliceID        []int32
	curSlice       int32
	cbp16          []int32 // MB ごと(bits0-3 luma cbp, 4-5 chroma, 6-7 chromaDC, 8 lumaDC)
	isI16          []bool  // I_16x16
	isIntra        []bool  // I_NxN or I_16x16(I スライスでは常に真)
	chromaPred     []int32 // chroma_pred_mode_table
	nz             *h264NZ // 4x4 ごとの非ゼロ数(luma/chroma AC)
	lastDqpNonzero bool
}

func newCabacMBState(mbW, mbH int) *cabacMBState {
	s := &cabacMBState{mbW: mbW, mbH: mbH,
		sliceID:    make([]int32, mbW*mbH),
		cbp16:      make([]int32, mbW*mbH),
		isI16:      make([]bool, mbW*mbH),
		isIntra:    make([]bool, mbW*mbH),
		chromaPred: make([]int32, mbW*mbH),
		nz:         newH264NZ(mbW, mbH),
	}
	for i := range s.sliceID {
		s.sliceID[i] = -1
	}
	return s
}

func (s *cabacMBState) avail(mb int) bool { return mb >= 0 && s.sliceID[mb] == s.curSlice }

// topCbp/leftCbp は cbf/cbp 文脈用の近傍 cbp(非MBAFF・フレーム・4:2:0)。
func (s *cabacMBState) topCbp(mb int) int32 {
	t := mb - s.mbW
	if s.avail(t) {
		return s.cbp16[t]
	}
	return 0x7CF // イントラで利用不可 → 全 coded 扱い
}

func (s *cabacMBState) leftCbp(mb int) int32 {
	if mb%s.mbW == 0 {
		return 0x7CF
	}
	l := mb - 1
	if !s.avail(l) {
		return 0x7CF
	}
	L := s.cbp16[l]
	return (L & 0x7F0) | ((L >> 0) & 2) | (((L >> 2) & 2) << 2)
}

// --- I スライス MB 走査 ---

// cabacISlice は1 I スライスのデータを走査する(sink 経由)。qpY はスライス QP。
// 成功で true。対象外(I_PCM 等)や末尾不整合で false。
func cabacISlice(sink cabacSink, st *cabacMBState, firstMB, total, qpY int) bool {
	st.curSlice++
	// 近傍 nz 追跡器のスライス識別子を歩調合わせ(cbf 文脈の可用性判定に必須)。
	st.nz.curSlice = st.curSlice
	curr := firstMB
	for {
		if curr >= total || sink.failed() {
			return false
		}
		st.sliceID[curr] = st.curSlice
		st.nz.sliceID[curr] = st.curSlice
		if !cabacIMB(sink, st, curr) {
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

// cabacIMB は1 I マクロブロックを走査する。
func cabacIMB(sink cabacSink, st *cabacMBState, mb int) bool {
	// mb_type(I スライス、ctx_base=3, intra_slice=1)
	ctx := 0
	if l := mb - 1; mb%st.mbW != 0 && st.avail(l) && st.isI16[l] {
		ctx++
	}
	if t := mb - st.mbW; st.avail(t) && st.isI16[t] {
		ctx++
	}
	st.isIntra[mb] = true
	i16 := false
	cbp := 0
	predMode := 0 // 0=I_NxN
	if sink.decision(3+ctx) == 0 {
		// I_NxN(I_4x4)
		predMode = 0
	} else {
		// terminate ビン → I_PCM
		if sink.terminate() == 1 {
			// I_PCM: 生画素 mbSize バイト(4:2:0/8bit=384)を消費し再初期化。
			const mbSize420 = 384
			if !sink.intraPCM(mbSize420) {
				return false
			}
			// 近傍状態: 全ブロック coded 扱い(FFmpeg: cbp_table=0xf7ef,
			// non_zero_count=16, chroma_pred=0, last_qscale_diff=0)。
			// 自前 cbp16 レイアウトで全 coded = luma0xf | chroma2(0x20)
			// | chromaDC(0xC0) | lumaDC(0x100) = 0x1EF。
			st.isI16[mb] = false
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
		i16 = true
		predMode = 1
		st.isI16[mb] = true
		// I_16x16。intra_mb_type は base=3, state+=2 後に相対 [1,2,3,4,5]
		// = 絶対 ctxIdx 6,7,8,9,10(FFmpeg decode_cabac_intra_mb_type)。
		mbt := 1
		mbt += 12 * sink.decision(6) // cbp_luma != 0
		if sink.decision(7) != 0 {   // cbp_chroma != 0
			mbt += 4 + 4*sink.decision(8) // cbp_chroma == 2
		}
		mbt += 2 * sink.decision(9)
		mbt += 1 * sink.decision(10)
		cbp = i16CBP(mbt)
	}
	_ = predMode

	if !i16 {
		// I_NxN: 16 個の prev_intra4x4_pred + rem
		for b := 0; b < 16; b++ {
			if sink.decision(68) == 0 { // prev_intra4x4_pred_mode_flag
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
		// coded_block_pattern: luma(4ビン、近傍文脈) + chroma
		cbp = cabacCBP(sink, st, mb)
	}
	st.cbp16[mb] = int32(cbp & 0x3F)

	// mb_qp_delta(cbp>0 または I16 のとき)。非コード時は last_qscale_diff=0
	// (次MBの ctx 用)。
	if cbp > 0 || i16 {
		cabacMBQPDelta(sink, st, mb)
	} else {
		st.lastDqpNonzero = false
	}

	// 残差
	return cabacIResidual(sink, st, mb, i16, cbp)
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
func cabacMBQPDelta(sink cabacSink, st *cabacMBState, mb int) {
	// ctxIdxInc = (直前MBの mb_qp_delta != 0) ? 1 : 0。近似として直前MBの
	// last_qscale_diff を追わず、ffmpeg と同じく「前MBの mb_qp_delta が非0か」を
	// cbp16 由来では判定できないため、専用フラグを持つ。
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
