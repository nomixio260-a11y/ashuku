package precomp

// HEVC の CABAC 構文走査(ピクセル復号なし、I/P/B スライス対応)。
//
// H.264 側(cabacmb.go)と同じ設計: ビン入出力を cabacSink 抽象に通し、
// 文脈選択(ctxIdx)と近傍状態だけを FFmpeg(=規格)と同一に再現する。
// 走査に必要な永続状態は CU 分割深さ(tab_ct_depth)・輝度イントラモード
// (tab_ipm)・cu_skip_flag のみ。動きベクトル・参照ピクチャ・マージ候補は
// 復号後の導出でありビットのパースには一切不要なので追わない。残差の
// CSBF・greater1 状態・Rice パラメータは TU ローカル。WPP は行頭での
// terminate+エントリポイント再初期化+前行 CTU2 個目後の文脈復元で追随。
//
// inter(P/B): cu_skip_flag / pred_mode / part_mode(AMP 含む)/ merge /
// inter_pred_idc / ref_idx / mvd_coding / mvp / rqt_root_cbf を復号する。
// v1 の対象: 4:2:0・タイルなし・長期参照/SCC なし。RExt 拡張(persistent
// rice、transform_skip_context 等)は非対応で、該当ストリームは走査が
// 破綻して検証で弾かれる(素通し保存に落ちる)。

// hevcTraceFn はデバッグ用トレースフック(テストから設定、通常 nil)。
var hevcTraceFn func(format string, args ...any)

func hevcTrace(format string, args ...any) {
	if hevcTraceFn != nil {
		hevcTraceFn(format, args...)
	}
}

const (
	hevcScanDiag  = 0
	hevcScanHoriz = 1
	hevcScanVert  = 2
)

const hevcIntraDC = 1

// バイパスビンの意味クラス(二次算術での文脈)。CABAC 上は等確率だが、
// 実分布には強い偏りがあるビン(Golomb-Rice の継続ビット等)を二次側で
// 文脈モデル化する。復号ビット列は不変なので検証シンクには影響しない。
const (
	hevcBypRemPrefix = 0  // +min(rice,4)*4+min(bit,3) → 0..19
	hevcBypMPM       = 20 // +bit(0/1) → 20,21
	hevcBypRemMode   = 22 // +bit(0..4) → 22..26
	hevcBypChroma    = 27 // +bit(0/1) → 27,28
	hevcBypMvdEG     = 29 // abs_mvd_minus2 EG1 継続、+min(k,8) → 29..37
	hevcBypMergeIdx  = 38 // merge_idx 単項継続、+min(pos,3) → 38..41
	hevcBypRefIdx    = 42 // ref_idx 単項継続、+min(pos,2) → 42..44
	hevcBypNumCls    = 45
)

// hevcClassedSink はクラス付きバイパスを二次算術で文脈符号化できるシンク。
type hevcClassedSink interface {
	bypassCls(cls int) int
}

// bypC はクラス付きバイパス(シンクが対応しなければ等確率)。
func (w *hevcWalk) bypC(cls int) int {
	if cs, ok := w.sink.(hevcClassedSink); ok {
		return cs.bypassCls(cls)
	}
	return w.sink.bypass()
}

// bypCPos は単項継続などの位置付きバイパス(base + min(pos,cap))。
func (w *hevcWalk) bypCPos(base, pos, cap int) int {
	if pos > cap {
		pos = cap
	}
	return w.bypC(base + pos)
}

// hevcCtxOps は文脈状態列の初期化/退避/復元(WPP)と、サブストリーム
// 境界での算術エンジン再初期化(bytePos は RBSP ボディ先頭からの
// バイト位置)を提供する。
type hevcCtxOps interface {
	initCtx(sliceQP, initType int)
	saveCtx()
	loadCtx()
	reinitEngine(bytePos int) bool
}

// hevcPicState はピクチャ単位の永続状態。
type hevcPicState struct {
	sps *hevcSPS
	pps *hevcPPS

	minCbW, minCbH int
	minPuW, minPuH int
	ctDepth        []uint8 // min-CB 粒度の分割深さ
	ipm            []uint8 // min-PU 粒度の輝度イントラモード
	skip           []uint8 // min-CB 粒度の cu_skip_flag(P/B の skip 文脈用)
}

func newHEVCPicState(sps *hevcSPS, pps *hevcPPS) *hevcPicState {
	log2MinPu := sps.log2MinCb - 1
	p := &hevcPicState{
		sps:    sps,
		pps:    pps,
		minCbW: sps.width >> sps.log2MinCb,
		minCbH: sps.height >> sps.log2MinCb,
		minPuW: sps.width >> log2MinPu,
		minPuH: sps.height >> log2MinPu,
	}
	p.ctDepth = make([]uint8, p.minCbW*p.minCbH)
	p.ipm = make([]uint8, p.minPuW*p.minPuH)
	p.skip = make([]uint8, p.minCbW*p.minCbH)
	return p
}

// hevcWalk は 1 スライスセグメント分の走査状態。
type hevcWalk struct {
	pic  *hevcPicState
	sl   *hevcSlice
	sink cabacSink
	ctx  hevcCtxOps

	sliceAddr int // 先頭 CTB の RS アドレス

	// CTU 単位
	ctbLeft, ctbUp bool

	// CU 単位(走査中の一時状態)
	tqBypass        bool
	intraSplit      bool
	cuIntra         bool // 現 CU が intra か
	interSplit0     bool // inter の depth0 強制分割条件
	curDepth        int  // 現 CU の四分木深さ(inter_pred_idc 文脈用)
	maxTrafoDepth   int
	puMode          [4]uint8
	cuModeC         uint8
	tuMode, tuModeC uint8
	isQpDeltaCoded  bool

	ok bool
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// hevcWalkSliceData はスライスデータ全体を走査する。呼び出し前に sink の
// 算術復号器/符号化器はスライスデータ先頭に位置している必要がある。
func hevcWalkSliceData(sink cabacSink, ctx hevcCtxOps, pic *hevcPicState, sl *hevcSlice) bool {
	sps := pic.sps
	w := &hevcWalk{pic: pic, sl: sl, sink: sink, ctx: ctx, ok: true}
	if !sl.firstSlice {
		w.sliceAddr = sl.segAddr
	}
	ctbW, ctbH := sps.ctbW, sps.ctbH
	wpp := pic.pps.entropyCodingSync

	// WPP: サブストリーム境界のバイト位置(エントリポイントの累積)。
	// スライスデータはヘッダ直後(バイト整列済み)から始まる。
	subOff := sl.headerBits / 8
	subIdx := 0

	ctx.initCtx(sl.sliceQP, hevcInitType(sl.sliceType, sl.cabacInitFlag))

	more := true
	addr := w.sliceAddr
	for more && addr < ctbW*ctbH {
		xCtb := (addr % ctbW) << sps.log2CtbSize
		yCtb := (addr / ctbW) << sps.log2CtbSize
		inSlice := addr - w.sliceAddr
		w.ctbLeft = xCtb > 0 && inSlice > 0
		w.ctbUp = yCtb > 0 && inSlice >= ctbW

		if wpp && addr != w.sliceAddr && addr%ctbW == 0 {
			// 行頭: end_of_subset_one_bit(terminate)を読み、次の
			// エントリポイントへ算術エンジンを再初期化。文脈は前行
			// CTU 2 個目後のスナップショットを復元する。
			if sink.terminate() != 1 {
				w.ok = false
				break
			}
			if subIdx >= len(sl.entryOffsets) {
				w.ok = false
				break
			}
			subOff += sl.entryOffsets[subIdx]
			subIdx++
			if !ctx.reinitEngine(subOff) {
				w.ok = false
				break
			}
			if ctbW == 1 {
				ctx.initCtx(sl.sliceQP, hevcInitType(sl.sliceType, sl.cabacInitFlag))
			} else {
				ctx.loadCtx()
			}
		}

		hevcTrace("CTB x=%d y=%d", xCtb, yCtb)
		if sps.sao {
			w.saoParam(xCtb>>sps.log2CtbSize, yCtb>>sps.log2CtbSize)
		}
		m := w.quadtree(xCtb, yCtb, sps.log2CtbSize, 0)
		if !w.ok || sink.failed() {
			return false
		}
		more = m > 0

		addr++
		if wpp && (addr%ctbW == 2 || (ctbW == 2 && addr%ctbW == 0)) {
			ctx.saveCtx()
		}
	}
	if more {
		// end_of_slice を見ずに CTB を使い切った(複数スライスの続きが
		// あるケース)。呼び出し側がスライス列全体で整合を確認する。
		_ = more
	}
	return w.ok && !sink.failed()
}

// --- SAO ---

func (w *hevcWalk) saoParam(rx, ry int) {
	s := w.sink
	sl := w.sl
	mergeLeft, mergeUp := false, false
	if sl.saoLuma || sl.saoChroma {
		if rx > 0 && w.ctbLeft {
			mergeLeft = s.decision(hevcCtxSaoMerge) == 1
		}
		if ry > 0 && !mergeLeft && w.ctbUp {
			mergeUp = s.decision(hevcCtxSaoMerge) == 1
		}
	}
	if mergeLeft || mergeUp {
		return
	}
	nc := 3
	if w.pic.sps.chromaFormatIDC == 0 {
		nc = 1
	}
	bd := w.pic.sps.bitDepth
	if bd > 10 {
		bd = 10
	}
	cMax := (1 << (bd - 5)) - 1
	typeCb := 0
	for c := 0; c < nc; c++ {
		enabled := sl.saoLuma
		if c > 0 {
			enabled = sl.saoChroma
		}
		if !enabled {
			continue
		}
		typ := 0
		if c == 2 {
			typ = typeCb
		} else {
			if s.decision(hevcCtxSaoTypeIdx) != 0 {
				if s.bypass() == 0 {
					typ = 1 // band
				} else {
					typ = 2 // edge
				}
			}
			if c == 1 {
				typeCb = typ
			}
		}
		if typ == 0 {
			continue
		}
		var abs [4]int
		for i := 0; i < 4; i++ {
			v := 0
			for v < cMax && s.bypass() == 1 {
				v++
			}
			abs[i] = v
		}
		if typ == 1 { // band: 符号+5bit 帯域位置
			for i := 0; i < 4; i++ {
				if abs[i] != 0 {
					s.bypass()
				}
			}
			for i := 0; i < 5; i++ {
				s.bypass()
			}
		} else if c != 2 { // edge: eo_class 2bit(Cr は Cb を再利用)
			s.bypass()
			s.bypass()
		}
	}
}

// --- 符号化四分木 ---

// quadtree は more_data(FFmpeg の戻り値そのまま)を返す。負値相当は
// w.ok=false で表現する。
func (w *hevcWalk) quadtree(x0, y0, log2, depth int) int {
	sps := w.pic.sps
	pps := w.pic.pps
	size := 1 << log2
	var split bool
	if x0+size <= sps.width && y0+size <= sps.height && log2 > sps.log2MinCb {
		split = w.sink.decision(hevcCtxSplitCU+w.splitCUCtx(x0, y0, depth)) == 1
	} else {
		split = log2 > sps.log2MinCb
	}
	if pps.cuQPDeltaEnabled && log2 >= sps.log2CtbSize-pps.diffCuQPDeltaDepth {
		w.isQpDeltaCoded = false
	}
	if split {
		half := size >> 1
		x1, y1 := x0+half, y0+half
		more := w.quadtree(x0, y0, log2-1, depth+1)
		if w.ok && more > 0 && x1 < sps.width {
			more = w.quadtree(x1, y0, log2-1, depth+1)
		}
		if w.ok && more > 0 && y1 < sps.height {
			more = w.quadtree(x0, y1, log2-1, depth+1)
		}
		if w.ok && more > 0 && x1 < sps.width && y1 < sps.height {
			more = w.quadtree(x1, y1, log2-1, depth+1)
		}
		if !w.ok {
			return 0
		}
		if more > 0 {
			if x1+half < sps.width || y1+half < sps.height {
				return 1
			}
			return 0
		}
		return more
	}
	w.codingUnit(x0, y0, log2, depth)
	if !w.ok {
		return 0
	}
	ctbSize := 1 << sps.log2CtbSize
	if ((x0+size)%ctbSize == 0 || x0+size >= sps.width) &&
		((y0+size)%ctbSize == 0 || y0+size >= sps.height) {
		eos := w.sink.terminate()
		hevcTrace("EOS v=%d", eos)
		if eos == 1 {
			return 0
		}
		return 1
	}
	return 1
}

func (w *hevcWalk) splitCUCtx(x0, y0, depth int) int {
	sps := w.pic.sps
	ctbMask := (1 << sps.log2CtbSize) - 1
	x0b, y0b := x0&ctbMask, y0&ctbMask
	xCb, yCb := x0>>sps.log2MinCb, y0>>sps.log2MinCb
	inc := 0
	if w.ctbLeft || x0b != 0 {
		if int(w.pic.ctDepth[yCb*w.pic.minCbW+xCb-1]) > depth {
			inc++
		}
	}
	if w.ctbUp || y0b != 0 {
		if int(w.pic.ctDepth[(yCb-1)*w.pic.minCbW+xCb]) > depth {
			inc++
		}
	}
	return inc
}

// --- CU ---

// part_mode の値(FFmpeg PART_* と同順)。
const (
	hevcPart2Nx2N = 0
	hevcPart2NxN  = 1
	hevcPartNx2N  = 2
	hevcPartNxN   = 3
	hevcPart2NxnU = 4
	hevcPart2NxnD = 5
	hevcPartNLx2N = 6
	hevcPartNRx2N = 7
)

func (w *hevcWalk) codingUnit(x0, y0, log2, depth int) {
	sps := w.pic.sps
	pps := w.pic.pps
	s := w.sink
	inter := w.sl.isInter()
	size := 1 << log2
	length := size >> sps.log2MinCb
	xCb, yCb := x0>>sps.log2MinCb, y0>>sps.log2MinCb

	hevcTrace("CU x=%d y=%d log2=%d", x0, y0, log2)
	w.intraSplit = false
	w.interSplit0 = false
	w.curDepth = depth
	w.tqBypass = false
	if pps.transquantBypass {
		w.tqBypass = s.decision(hevcCtxTQBypass) == 1
	}

	skip := false
	if inter {
		skip = s.decision(hevcCtxSkipFlag+w.skipCtx(x0, y0)) == 1
		hevcTrace("SKIP v=%d", b2i(skip))
	}
	// skip_flag を近傍参照のため即時に CU 全域へ書き戻す
	for y := 0; y < length; y++ {
		row := (yCb+y)*w.pic.minCbW + xCb
		for x := 0; x < length; x++ {
			w.pic.skip[row+x] = uint8(b2i(skip))
		}
	}

	if skip {
		w.cuIntra = false
		w.fillIPM(x0, y0, size, hevcIntraDC)
		w.predictionUnit(size, size, 0, true)
	} else {
		predIntra := true
		if inter {
			predIntra = s.decision(hevcCtxPredMode) == 1
			hevcTrace("PMODE v=%d", b2i(predIntra))
		}
		w.cuIntra = predIntra
		part := hevcPart2Nx2N
		if !predIntra || log2 == sps.log2MinCb {
			part = w.partMode(log2, predIntra)
			hevcTrace("PART p=%d intra=%d", part, b2i(predIntra))
		}
		w.intraSplit = part == hevcPartNxN && predIntra

		pcm := false
		mergeFlag := false
		if predIntra {
			if part == hevcPart2Nx2N && sps.pcmEnabled &&
				log2 >= sps.log2MinPcm && log2 <= sps.log2MaxPcm {
				pcm = s.terminate() == 1
			}
			hevcTrace("CUI part=%d pcm=%d", part, b2i(pcm))
			if pcm {
				w.fillIPM(x0, y0, size, hevcIntraDC)
				bits := size*size*sps.pcmBitDepthLuma +
					2*(size>>1)*(size>>1)*sps.pcmBitDepthChr
				if !s.intraPCM((bits + 7) >> 3) {
					w.ok = false
					return
				}
			} else {
				w.intraPredictionUnit(x0, y0, log2)
			}
		} else {
			w.fillIPM(x0, y0, size, hevcIntraDC)
			mergeFlag = w.interDispatch(x0, y0, log2, part)
		}
		if !w.ok {
			return
		}
		if !pcm {
			rqtRoot := true
			if !predIntra && !(part == hevcPart2Nx2N && mergeFlag) {
				rqtRoot = s.decision(hevcCtxNoResidual) == 1
				hevcTrace("RQT v=%d", b2i(rqtRoot))
			}
			if rqtRoot {
				if predIntra {
					w.maxTrafoDepth = sps.maxTrDepthIntra + b2i(w.intraSplit)
				} else {
					w.maxTrafoDepth = sps.maxTrDepthInter
					w.interSplit0 = sps.maxTrDepthInter == 0 && part != hevcPart2Nx2N
				}
				w.transformTree(x0, y0, x0, y0, log2, 0, 0, 0, 0)
				if !w.ok {
					return
				}
			}
		}
	}
	// CU 全域に分割深さを書き戻す
	for y := 0; y < length; y++ {
		row := (yCb+y)*w.pic.minCbW + xCb
		for x := 0; x < length; x++ {
			w.pic.ctDepth[row+x] = uint8(depth)
		}
	}
}

// skipCtx は cu_skip_flag の文脈 inc(左/上 CB の skip)。
func (w *hevcWalk) skipCtx(x0, y0 int) int {
	sps := w.pic.sps
	ctbMask := (1 << sps.log2CtbSize) - 1
	x0b, y0b := x0&ctbMask, y0&ctbMask
	xCb, yCb := x0>>sps.log2MinCb, y0>>sps.log2MinCb
	inc := 0
	if (w.ctbLeft || x0b != 0) && w.pic.skip[yCb*w.pic.minCbW+xCb-1] != 0 {
		inc++
	}
	if (w.ctbUp || y0b != 0) && w.pic.skip[(yCb-1)*w.pic.minCbW+xCb] != 0 {
		inc++
	}
	return inc
}

// partMode は part_mode を復号する(FFmpeg ff_hevc_part_mode_decode)。
func (w *hevcWalk) partMode(log2 int, intra bool) int {
	s := w.sink
	sps := w.pic.sps
	if s.decision(hevcCtxPartMode) == 1 {
		return hevcPart2Nx2N
	}
	if log2 == sps.log2MinCb {
		if intra {
			return hevcPartNxN
		}
		if s.decision(hevcCtxPartMode+1) == 1 {
			return hevcPart2NxN
		}
		if log2 == 3 {
			return hevcPartNx2N
		}
		if s.decision(hevcCtxPartMode+2) == 1 {
			return hevcPartNx2N
		}
		return hevcPartNxN
	}
	if !sps.amp {
		if s.decision(hevcCtxPartMode+1) == 1 {
			return hevcPart2NxN
		}
		return hevcPartNx2N
	}
	if s.decision(hevcCtxPartMode+1) == 1 {
		if s.decision(hevcCtxPartMode+3) == 1 {
			return hevcPart2NxN
		}
		if s.bypass() == 1 {
			return hevcPart2NxnD
		}
		return hevcPart2NxnU
	}
	if s.decision(hevcCtxPartMode+3) == 1 {
		return hevcPartNx2N
	}
	if s.bypass() == 1 {
		return hevcPartNRx2N
	}
	return hevcPartNLx2N
}

// interDispatch は part_mode に応じて各 PU を予測パースし、最後の PU の
// merge_flag を返す(rqt_root_cbf 条件用)。
func (w *hevcWalk) interDispatch(x0, y0, log2, part int) bool {
	cb := 1 << log2
	last := false
	pu := func(nPbW, nPbH, idx int) {
		if w.ok {
			last = w.predictionUnit(nPbW, nPbH, idx, false)
		}
	}
	switch part {
	case hevcPart2Nx2N:
		pu(cb, cb, 0)
	case hevcPart2NxN:
		pu(cb, cb/2, 0)
		pu(cb, cb/2, 1)
	case hevcPartNx2N:
		pu(cb/2, cb, 0)
		pu(cb/2, cb, 1)
	case hevcPart2NxnU:
		pu(cb, cb/4, 0)
		pu(cb, cb*3/4, 1)
	case hevcPart2NxnD:
		pu(cb, cb*3/4, 0)
		pu(cb, cb/4, 1)
	case hevcPartNLx2N:
		pu(cb/4, cb, 0)
		pu(cb*3/4, cb, 1)
	case hevcPartNRx2N:
		pu(cb*3/4, cb, 0)
		pu(cb/4, cb, 1)
	case hevcPartNxN:
		pu(cb/2, cb/2, 0)
		pu(cb/2, cb/2, 1)
		pu(cb/2, cb/2, 2)
		pu(cb/2, cb/2, 3)
	}
	return last
}

// predictionUnit は 1 PU の inter 予測構文を復号し merge_flag を返す。
func (w *hevcWalk) predictionUnit(nPbW, nPbH, partIdx int, skip bool) bool {
	s := w.sink
	sl := w.sl
	mergeFlag := skip
	if !skip {
		mergeFlag = s.decision(hevcCtxMergeFlag) == 1
	}
	if skip || mergeFlag {
		idx := 0
		if sl.maxNumMerge > 1 {
			idx = s.decision(hevcCtxMergeIdx)
			if idx != 0 {
				for idx < sl.maxNumMerge-1 && w.bypCPos(hevcBypMergeIdx, idx-1, 3) == 1 {
					idx++
				}
			}
		}
		hevcTrace("MRG f=1 idx=%d", idx)
		return mergeFlag
	}
	hevcTrace("MRG f=0")
	// AMVP: inter_pred_idc(B のみ)→ L0/L1 それぞれ ref_idx/mvd/mvp
	idc := 0 // PRED_L0
	if sl.isB() {
		idc = w.interPredIdc(nPbW, nPbH)
	}
	hevcTrace("IPD idc=%d", idc)
	if idc != 1 { // != PRED_L1 → L0 or BI
		w.refIdx(0, sl.numRefIdx[0])
		w.mvdCoding()
		mvp := s.decision(hevcCtxMvpFlag)
		hevcTrace("MVP l=0 v=%d", mvp)
	}
	if idc != 0 { // != PRED_L0 → L1 or BI
		w.refIdx(1, sl.numRefIdx[1])
		if !(sl.mvdL1Zero && idc == 2) {
			w.mvdCoding()
		}
		mvp := s.decision(hevcCtxMvpFlag)
		hevcTrace("MVP l=1 v=%d", mvp)
	}
	return mergeFlag
}

// interPredIdc は inter_pred_idc を復号(0=L0,1=L1,2=BI)。
func (w *hevcWalk) interPredIdc(nPbW, nPbH int) int {
	s := w.sink
	if nPbW+nPbH == 12 {
		if s.decision(hevcCtxInterPredIdc+4) == 1 {
			return 1
		}
		return 0
	}
	// ct_depth 文脈: 現 CU の四分木深さ。ct_depth は quadtree の depth に一致。
	if s.decision(hevcCtxInterPredIdc+w.curDepth) == 1 {
		return 2 // PRED_BI
	}
	if s.decision(hevcCtxInterPredIdc+4) == 1 {
		return 1
	}
	return 0
}

// refIdx は ref_idx_lX を復号(先頭 min(max,2) bin が文脈、以降 bypass)。
func (w *hevcWalk) refIdx(list, numRef int) {
	s := w.sink
	max := numRef - 1
	i := 0
	if max > 0 {
		maxCtx := max
		if maxCtx > 2 {
			maxCtx = 2
		}
		for i < maxCtx && s.decision(hevcCtxRefIdx+i) == 1 {
			i++
		}
		if i == 2 {
			for i < max && w.bypCPos(hevcBypRefIdx, i-2, 2) == 1 {
				i++
			}
		}
	}
	hevcTrace("REF l=%d idx=%d", list, i)
}

// mvdCoding は mvd_coding を復号(x/y 2 成分)。
func (w *hevcWalk) mvdCoding() {
	s := w.sink
	gt0x := s.decision(hevcCtxAbsMvdGt0)
	gt0y := s.decision(hevcCtxAbsMvdGt0)
	xv, yv := gt0x, gt0y
	if gt0x == 1 {
		xv += s.decision(hevcCtxAbsMvdGt1 + 1)
	}
	if gt0y == 1 {
		yv += s.decision(hevcCtxAbsMvdGt1 + 1)
	}
	w.mvdComponent(xv)
	w.mvdComponent(yv)
	hevcTrace("MVD x=%d y=%d", xv, yv)
}

// mvdComponent は 1 成分の残り(abs_mvd_minus2 EG1 + 符号、または符号のみ)。
func (w *hevcWalk) mvdComponent(v int) {
	s := w.sink
	switch v {
	case 2: // abs_mvd_minus2: EG1 bypass prefix(継続=偏り)+suffix+符号
		k := 1
		for k < 31 && w.bypCPos(hevcBypMvdEG, k-1, 8) == 1 {
			k++
		}
		if k >= 31 {
			w.ok = false
			return
		}
		for k > 0 {
			k--
			s.bypass() // suffix ビット(ほぼ等確率)
		}
		s.bypass() // sign
	case 1:
		s.bypass() // sign
	}
}

func (w *hevcWalk) fillIPM(x0, y0, size int, mode uint8) {
	sps := w.pic.sps
	log2MinPu := sps.log2MinCb - 1
	n := size >> log2MinPu
	if n == 0 {
		n = 1
	}
	xPu, yPu := x0>>log2MinPu, y0>>log2MinPu
	for j := 0; j < n; j++ {
		row := (yPu+j)*w.pic.minPuW + xPu
		for i := 0; i < n; i++ {
			w.pic.ipm[row+i] = mode
		}
	}
}

// --- イントラ予測モード ---

var hevcIntraChromaTable = [4]uint8{0, 26, 10, 1}

func (w *hevcWalk) intraPredictionUnit(x0, y0, log2 int) {
	s := w.sink
	side := 1
	if w.intraSplit {
		side = 2
	}
	pbSize := (1 << log2) >> (side - 1)
	var prevFlag [4]bool
	for i := 0; i < side; i++ {
		for j := 0; j < side; j++ {
			prevFlag[2*i+j] = s.decision(hevcCtxPrevIntraLuma) == 1
		}
	}
	for i := 0; i < side; i++ {
		for j := 0; j < side; j++ {
			mpmIdx, remMode := 0, 0
			if prevFlag[2*i+j] {
				for mpmIdx < 2 && w.bypC(hevcBypMPM+mpmIdx) == 1 {
					mpmIdx++
				}
			} else {
				for k := 0; k < 5; k++ {
					remMode = remMode<<1 | w.bypC(hevcBypRemMode+k)
				}
			}
			w.puMode[2*i+j] = w.lumaIntraPredMode(x0+pbSize*j, y0+pbSize*i,
				pbSize, prevFlag[2*i+j], mpmIdx, remMode)
			hevcTrace("IPM pb=%d prev=%d mode=%d", 2*i+j, b2i(prevFlag[2*i+j]), w.puMode[2*i+j])
		}
	}
	// 4:2:0: CU あたり 1 つの色差モード
	var chromaMode int
	if s.decision(hevcCtxIntraChroma) == 0 {
		chromaMode = 4
	} else {
		chromaMode = w.bypC(hevcBypChroma) << 1
		chromaMode |= w.bypC(hevcBypChroma + 1)
	}
	hevcTrace("CHR cm=%d", chromaMode)
	if chromaMode != 4 {
		t := hevcIntraChromaTable[chromaMode]
		if w.puMode[0] == t {
			w.cuModeC = 34
		} else {
			w.cuModeC = t
		}
	} else {
		w.cuModeC = w.puMode[0]
	}
}

func (w *hevcWalk) lumaIntraPredMode(x0, y0, puSize int, prev bool, mpmIdx, remMode int) uint8 {
	sps := w.pic.sps
	log2MinPu := sps.log2MinCb - 1
	xPu, yPu := x0>>log2MinPu, y0>>log2MinPu
	ctbMask := (1 << sps.log2CtbSize) - 1
	x0b, y0b := x0&ctbMask, y0&ctbMask

	candUp, candLeft := hevcIntraDC, hevcIntraDC
	if w.ctbUp || y0b != 0 {
		candUp = int(w.pic.ipm[(yPu-1)*w.pic.minPuW+xPu])
	}
	if w.ctbLeft || x0b != 0 {
		candLeft = int(w.pic.ipm[yPu*w.pic.minPuW+xPu-1])
	}
	// モード予測は CTB 行境界を越えない
	yCtb := (y0 >> sps.log2CtbSize) << sps.log2CtbSize
	if y0-1 < yCtb {
		candUp = hevcIntraDC
	}

	var cand [3]int
	if candLeft == candUp {
		if candLeft < 2 {
			cand = [3]int{0, 1, 26} // PLANAR, DC, ANGULAR_26
		} else {
			cand[0] = candLeft
			cand[1] = 2 + ((candLeft - 2 - 1 + 32) & 31)
			cand[2] = 2 + ((candLeft - 2 + 1) & 31)
		}
	} else {
		cand[0], cand[1] = candLeft, candUp
		if cand[0] != 0 && cand[1] != 0 {
			cand[2] = 0
		} else if cand[0] != 1 && cand[1] != 1 {
			cand[2] = 1
		} else {
			cand[2] = 26
		}
	}

	var mode int
	if prev {
		mode = cand[mpmIdx]
	} else {
		if cand[0] > cand[1] {
			cand[0], cand[1] = cand[1], cand[0]
		}
		if cand[0] > cand[2] {
			cand[0], cand[2] = cand[2], cand[0]
		}
		if cand[1] > cand[2] {
			cand[1], cand[2] = cand[2], cand[1]
		}
		mode = remMode
		for i := 0; i < 3; i++ {
			if mode >= cand[i] {
				mode++
			}
		}
	}
	w.fillIPM(x0, y0, puSize, uint8(mode))
	return uint8(mode)
}

// --- 変換木 ---

func (w *hevcWalk) transformTree(x0, y0, xBase, yBase, log2, depth, blkIdx, baseCbfCb, baseCbfCr int) {
	sps := w.pic.sps
	s := w.sink
	cbfCb, cbfCr := baseCbfCb, baseCbfCr

	if w.cuIntra {
		if w.intraSplit {
			if depth == 1 {
				w.tuMode = w.puMode[blkIdx]
				w.tuModeC = w.cuModeC
			}
		} else {
			w.tuMode = w.puMode[0]
			w.tuModeC = w.cuModeC
		}
	}

	var split bool
	if log2 <= sps.log2MaxTb && log2 > sps.log2MinTb &&
		depth < w.maxTrafoDepth && !(w.intraSplit && depth == 0) {
		split = s.decision(hevcCtxSplitTrafo+5-log2) == 1
	} else {
		interSplit := w.interSplit0 && depth == 0
		split = log2 > sps.log2MaxTb || (w.intraSplit && depth == 0) || interSplit
	}

	if sps.chromaFormatIDC != 0 && log2 > 2 {
		if depth == 0 || cbfCb != 0 {
			cbfCb = s.decision(hevcCtxCbfCbCr + depth)
		}
		if depth == 0 || cbfCr != 0 {
			cbfCr = s.decision(hevcCtxCbfCbCr + depth)
		}
	}

	hevcTrace("TT x=%d y=%d log2=%d d=%d sp=%d cb=%d cr=%d", x0, y0, log2, depth, b2i(split), cbfCb, cbfCr)
	if split {
		half := 1 << (log2 - 1)
		x1, y1 := x0+half, y0+half
		w.transformTree(x0, y0, x0, y0, log2-1, depth+1, 0, cbfCb, cbfCr)
		if w.ok {
			w.transformTree(x1, y0, x0, y0, log2-1, depth+1, 1, cbfCb, cbfCr)
		}
		if w.ok {
			w.transformTree(x0, y1, x0, y0, log2-1, depth+1, 2, cbfCb, cbfCr)
		}
		if w.ok {
			w.transformTree(x1, y1, x0, y0, log2-1, depth+1, 3, cbfCb, cbfCr)
		}
		return
	}

	// cbf_luma: intra は常に復号。inter は depth!=0 か色差 cbf があるとき
	// のみ復号し、depth0 で色差 cbf 無しなら暗黙 1。
	cbfLuma := 1
	if w.cuIntra || depth != 0 || cbfCb != 0 || cbfCr != 0 {
		cbfLuma = s.decision(hevcCtxCbfLuma + b2i(depth == 0))
	}
	hevcTrace("TU x=%d y=%d log2=%d d=%d blk=%d cbfL=%d", x0, y0, log2, depth, blkIdx, cbfLuma)
	w.transformUnit(x0, y0, xBase, yBase, log2, blkIdx, cbfLuma, cbfCb, cbfCr)
}

// --- TU ---

func (w *hevcWalk) transformUnit(x0, y0, xBase, yBase, log2, blkIdx, cbfLuma, cbfCb, cbfCr int) {
	sps := w.pic.sps
	pps := w.pic.pps
	s := w.sink

	if cbfLuma == 0 && cbfCb == 0 && cbfCr == 0 {
		return
	}
	if pps.cuQPDeltaEnabled && !w.isQpDeltaCoded {
		w.cuQPDelta()
		w.isQpDeltaCoded = true
	}
	// cu_chroma_qp_offset は RExt(chroma_qp_offset_list)のみ: v1 対象外

	// スキャン選択はモード依存だが inter では常に DIAG。
	scanIdx, scanIdxC := hevcScanDiag, hevcScanDiag
	if w.cuIntra && log2 < 4 {
		if w.tuMode >= 6 && w.tuMode <= 14 {
			scanIdx = hevcScanVert
		} else if w.tuMode >= 22 && w.tuMode <= 30 {
			scanIdx = hevcScanHoriz
		}
		if w.tuModeC >= 6 && w.tuModeC <= 14 {
			scanIdxC = hevcScanVert
		} else if w.tuModeC >= 22 && w.tuModeC <= 30 {
			scanIdxC = hevcScanHoriz
		}
	}

	if cbfLuma != 0 {
		w.residual(log2, scanIdx, 0)
	}
	if !w.ok {
		return
	}
	if sps.chromaFormatIDC != 0 && log2 > 2 {
		if cbfCb != 0 {
			w.residual(log2-1, scanIdxC, 1)
		}
		if w.ok && cbfCr != 0 {
			w.residual(log2-1, scanIdxC, 2)
		}
	} else if sps.chromaFormatIDC != 0 && blkIdx == 3 {
		// 4x4 輝度 4 つの親(8x8)分の色差を最後のブロックでまとめて復号
		if cbfCb != 0 {
			w.residual(log2, scanIdxC, 1)
		}
		if w.ok && cbfCr != 0 {
			w.residual(log2, scanIdxC, 2)
		}
	}
	_ = s
}

func (w *hevcWalk) cuQPDelta() {
	s := w.sink
	prefix := 0
	inc := 0
	for prefix < 5 && s.decision(hevcCtxQPDelta+inc) == 1 {
		prefix++
		inc = 1
	}
	val := prefix
	if prefix >= 5 {
		// EG0 サフィックス
		k := 0
		suffix := 0
		for k < 7 && s.bypass() == 1 {
			suffix += 1 << k
			k++
		}
		if k == 7 {
			w.ok = false
			return
		}
		for k > 0 {
			k--
			suffix += s.bypass() << k
		}
		val += suffix
	}
	if val != 0 {
		if s.bypass() == 1 {
			val = -val
		}
	}
	hevcTrace("DQP v=%d", val)
}

// --- 残差 ---

func (w *hevcWalk) residual(log2, scanIdx, cIdx int) {
	pps := w.pic.pps
	s := w.sink

	// transform_skip_flag(Main: 4x4 のみ)
	if !w.tqBypass && pps.transformSkip && log2 <= 2 {
		s.decision(hevcCtxTransformSkip + b2i(cIdx > 0))
	}

	// last_sig_coeff x/y prefix + suffix
	maxP := log2<<1 - 1
	var ctxOff, ctxShift int
	if cIdx == 0 {
		ctxOff = 3*(log2-2) + ((log2 - 1) >> 2)
		ctxShift = (log2 + 1) >> 2
	} else {
		ctxOff = 15
		ctxShift = log2 - 2
	}
	lastX := 0
	for lastX < maxP && s.decision(hevcCtxLastXPrefix+(lastX>>ctxShift)+ctxOff) == 1 {
		lastX++
	}
	lastY := 0
	for lastY < maxP && s.decision(hevcCtxLastYPrefix+(lastY>>ctxShift)+ctxOff) == 1 {
		lastY++
	}
	if lastX > 3 {
		length := lastX>>1 - 1
		v := s.bypass()
		for i := 1; i < length; i++ {
			v = v<<1 | s.bypass()
		}
		lastX = (1<<(lastX>>1-1))*(2+(lastX&1)) + v
	}
	if lastY > 3 {
		length := lastY>>1 - 1
		v := s.bypass()
		for i := 1; i < length; i++ {
			v = v<<1 | s.bypass()
		}
		lastY = (1<<(lastY>>1-1))*(2+(lastY&1)) + v
	}
	if scanIdx == hevcScanVert {
		lastX, lastY = lastY, lastX
	}

	hevcTrace("RES c=%d log2=%d sc=%d last=%d,%d", cIdx, log2, scanIdx, lastX, lastY)
	xCgLast, yCgLast := lastX>>2, lastY>>2
	trafoSize := 1 << log2

	// CG 走査テーブルと係数総数
	var scanXCg, scanYCg []uint8
	numCoeff := 0
	switch scanIdx {
	case hevcScanDiag:
		numCoeff = int(hevcDiagScan4x4Inv[lastY&3][lastX&3])
		switch trafoSize {
		case 4:
			scanXCg = []uint8{0}
			scanYCg = []uint8{0}
		case 8:
			numCoeff += int(hevcDiagScan2x2Inv[yCgLast][xCgLast]) << 4
			scanXCg = []uint8{0, 0, 1, 1}
			scanYCg = []uint8{0, 1, 0, 1}
		case 16:
			numCoeff += int(hevcDiagScan4x4Inv[yCgLast][xCgLast]) << 4
			scanXCg = hevcDiagScan4x4X[:]
			scanYCg = hevcDiagScan4x4Y[:]
		default: // 32
			numCoeff += int(hevcDiagScan8x8Inv[yCgLast][xCgLast]) << 4
			scanXCg = hevcDiagScan8x8X[:]
			scanYCg = hevcDiagScan8x8Y[:]
		}
	case hevcScanHoriz:
		scanXCg = hevcHorizScan2x2X[:]
		scanYCg = hevcHorizScan2x2Y[:]
		numCoeff = int(hevcHorizScan8x8Inv[lastY][lastX])
	default: // vert
		scanXCg = hevcHorizScan2x2Y[:]
		scanYCg = hevcHorizScan2x2X[:]
		numCoeff = int(hevcHorizScan8x8Inv[lastX][lastY])
	}
	numCoeff++
	numLastSubset := (numCoeff - 1) >> 4

	var csbf [8][8]uint8
	greater1Ctx := 1

	for i := numLastSubset; i >= 0; i-- {
		implicitNonZero := false
		offset := i << 4
		xCg, yCg := int(scanXCg[i]), int(scanYCg[i])
		gridMax := 1<<(log2-2) - 1

		if i < numLastSubset && i > 0 {
			ctxCg := 0
			if xCg < gridMax {
				ctxCg += int(csbf[xCg+1][yCg])
			}
			if yCg < gridMax {
				ctxCg += int(csbf[xCg][yCg+1])
			}
			inc := ctxCg
			if inc > 1 {
				inc = 1
			}
			if cIdx > 0 {
				inc += 2
			}
			csbf[xCg][yCg] = uint8(s.decision(hevcCtxSigGroup + inc))
			implicitNonZero = true
		} else {
			v := uint8(0)
			if (xCg == xCgLast && yCg == yCgLast) || (xCg == 0 && yCg == 0) {
				v = 1
			}
			csbf[xCg][yCg] = v
		}

		lastScanPos := numCoeff - offset - 1
		var sigIdx [16]int
		nb := 0
		nEnd := 15
		if i == numLastSubset {
			nEnd = lastScanPos - 1
			sigIdx[0] = lastScanPos
			nb = 1
		}

		prevSig := 0
		if xCg < gridMax && csbf[xCg+1][yCg] != 0 {
			prevSig = 1
		}
		if yCg < gridMax && csbf[xCg][yCg+1] != 0 {
			prevSig += 2
		}

		if csbf[xCg][yCg] != 0 && nEnd >= 0 {
			// 文脈マップと scf_offset の選択
			scfOffset := 0
			var mapP []uint8
			if cIdx != 0 {
				scfOffset = 27
			}
			if log2 == 2 {
				mapP = hevcSigCtxIdxMap[scanIdx][0:16]
			} else {
				base := (prevSig + 1) << 4
				mapP = hevcSigCtxIdxMap[scanIdx][base : base+16]
				if cIdx == 0 {
					if xCg > 0 || yCg > 0 {
						scfOffset += 3
					}
					if log2 == 3 {
						if scanIdx == hevcScanDiag {
							scfOffset += 9
						} else {
							scfOffset += 15
						}
					} else {
						scfOffset += 21
					}
				} else {
					if log2 == 3 {
						scfOffset += 9
					} else {
						scfOffset += 12
					}
				}
			}
			nb0 := nb
			for n := nEnd; n > 0; n-- {
				sig := s.decision(hevcCtxSigCoeff + int(mapP[n]) + scfOffset)
				sigIdx[nb] = n
				nb += sig
			}
			if nb != nb0 {
				implicitNonZero = false
			}
			if !implicitNonZero {
				// DC 位置は明示ビン
				if i == 0 {
					if cIdx == 0 {
						scfOffset = 0
					} else {
						scfOffset = 27
					}
				} else {
					scfOffset = 2 + scfOffset
				}
				sigIdx[nb] = 0
				nb += s.decision(hevcCtxSigCoeff + scfOffset)
			} else {
				sigIdx[nb] = 0
				nb++
			}
		}

		hevcTrace("CG i=%d csbf=%d nb=%d", i, csbf[xCg][yCg], nb)
		if nb == 0 {
			continue
		}

		// greater1 / greater2
		ctxSet := 0
		if i > 0 && cIdx == 0 {
			ctxSet = 2
		}
		if i != numLastSubset && greater1Ctx == 0 {
			ctxSet++
		}
		greater1Ctx = 1
		lastNZ := sigIdx[0]
		firstNZ := sigIdx[nb-1]

		var gt1 [8]int
		firstG1 := -1
		ng1 := nb
		if ng1 > 8 {
			ng1 = 8
		}
		for m := 0; m < ng1; m++ {
			inc := ctxSet<<2 + greater1Ctx
			if cIdx > 0 {
				inc += 16
			}
			flag := s.decision(hevcCtxGreater1 + inc)
			gt1[m] = flag
			if flag == 1 {
				greater1Ctx = 0
				if firstG1 < 0 {
					firstG1 = m
				}
			} else if greater1Ctx > 0 && greater1Ctx < 3 {
				greater1Ctx++
			}
		}

		signHidden := false
		if !w.tqBypass {
			signHidden = lastNZ-firstNZ >= 4
		}
		if firstG1 >= 0 {
			inc := ctxSet
			if cIdx > 0 {
				inc += 4
			}
			gt1[firstG1] += s.decision(hevcCtxGreater2 + inc)
		}

		nSigns := nb
		if pps.signDataHiding && signHidden {
			nSigns = nb - 1
		}
		for k := 0; k < nSigns; k++ {
			s.bypass()
		}

		// レベル復元(Rice パラメータ適応のため値まで追う)
		rice := 0
		for m := 0; m < nb; m++ {
			var level int
			if m < 8 {
				level = 1 + gt1[m]
				cap := 2
				if m == firstG1 {
					cap = 3
				}
				if level == cap {
					rem := w.coeffAbsLevelRemaining(rice)
					level += rem
					if level > 3<<rice && rice < 4 {
						rice++
					}
				}
			} else {
				rem := w.coeffAbsLevelRemaining(rice)
				level = 1 + rem
				if level > 3<<rice && rice < 4 {
					rice++
				}
			}
			if !w.ok {
				return
			}
		}
	}
}

// coeffAbsLevelRemaining は Golomb-Rice + 指数ゴロムのバイパス列。
func (w *hevcWalk) coeffAbsLevelRemaining(rice int) int {
	s := w.sink
	rcls := rice
	if rcls > 4 {
		rcls = 4
	}
	prefix := 0
	for prefix < 31 {
		bcls := prefix
		if bcls > 3 {
			bcls = 3
		}
		if w.bypC(hevcBypRemPrefix+rcls*4+bcls) == 0 {
			break
		}
		prefix++
	}
	if prefix < 3 {
		suffix := 0
		for i := 0; i < rice; i++ {
			suffix = suffix<<1 | s.bypass()
		}
		return prefix<<rice + suffix
	}
	if prefix == 31 || prefix-3+rice > 22 {
		w.ok = false
		return 0
	}
	k := prefix - 3 + rice
	suffix := 0
	for i := 0; i < k; i++ {
		suffix = suffix<<1 | s.bypass()
	}
	return (1<<(prefix-3)+2)<<rice + suffix
}
