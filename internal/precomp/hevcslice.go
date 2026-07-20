package precomp

// HEVC スライスセグメントヘッダの解析(parse-only、I スライス中心)。

// hevcSlice はスライスヘッダの解析結果。
type hevcSlice struct {
	firstSlice    bool
	dependent     bool
	segAddr       int
	sliceType     int // 0=B 1=P 2=I
	sliceQP       int
	saoLuma       bool
	saoChroma     bool
	cabacInitFlag bool
	numEntry      int
	entryOffsets  []int // entry_point_offset(+1 済み、バイト)
	headerBits    int   // NAL ヘッダ(2バイト)後からヘッダ末尾(整列後)までのビット数
	// inter(P/B)用
	numRefIdx   [2]int // num_ref_idx_lX_active(L0/L1)
	maxNumMerge int    // MaxNumMergeCand(1..5)
	mvdL1Zero   bool   // mvd_l1_zero_flag
	temporalMvp bool   // slice_temporal_mvp_enabled_flag
}

func (sl *hevcSlice) isInter() bool { return sl.sliceType != 2 }
func (sl *hevcSlice) isB() bool     { return sl.sliceType == 0 }

// ceilLog2 は av_ceil_log2 相当。
func ceilLog2(n int) int {
	b := 0
	for 1<<uint(b) < n {
		b++
	}
	return b
}

// parseHEVCSliceHeader はスライスヘッダを解析する。r は RBSP の NAL ヘッダ
// 2バイト直後を指す。nalType は NAL タイプ。
func parseHEVCSliceHeader(r *h264Reader, sps *hevcSPS, pps *hevcPPS, nalType int) (*hevcSlice, bool) {
	sl := &hevcSlice{}
	b, err := r.u1()
	if err != nil {
		return nil, false
	}
	sl.firstSlice = b == 1
	if hevcIsIRAP(nalType) {
		if _, err := r.u1(); err != nil { // no_output_of_prior_pics
			return nil, false
		}
	}
	if _, err := r.ue(); err != nil { // pps_id(呼び出し側で解決済み)
		return nil, false
	}
	if !sl.firstSlice {
		if pps.dependentSlices {
			b, err := r.u1()
			if err != nil {
				return nil, false
			}
			sl.dependent = b == 1
		}
		addrBits := ceilLog2(sps.ctbW * sps.ctbH)
		v, err := r.u(addrBits)
		if err != nil {
			return nil, false
		}
		sl.segAddr = int(v)
	}
	if sl.dependent {
		return nil, false // dependent slice segment は対象外(v1)
	}
	for i := 0; i < pps.numExtraSliceBits; i++ {
		if _, err := r.u1(); err != nil {
			return nil, false
		}
	}
	st, err := r.ue()
	if err != nil || st > 2 {
		return nil, false
	}
	sl.sliceType = int(st)
	if pps.outputFlagPresent {
		if _, err := r.u1(); err != nil {
			return nil, false
		}
	}
	numPocTotalCurr := 0
	isIDR := nalType == hevcNALIDRWRadl || nalType == hevcNALIDRNLP
	if !isIDR {
		if _, err := r.u(sps.log2MaxPocLsb); err != nil { // poc_lsb
			return nil, false
		}
		spsFlag, err := r.u1() // short_term_ref_pic_set_sps_flag
		if err != nil {
			return nil, false
		}
		if spsFlag == 0 {
			// スライス内 st_ref_pic_set(idx = num_short_term_ref_pic_sets)
			idx := len(sps.numDeltaPocs)
			_, used, ok := hevcParseShortTermRPSSlice(r, idx, sps.numDeltaPocs)
			if !ok {
				return nil, false
			}
			numPocTotalCurr = used
		} else if len(sps.numDeltaPocs) > 0 {
			nb := ceilLog2(len(sps.numDeltaPocs))
			rpsIdx := 0
			if nb > 0 {
				v, err := r.u(nb)
				if err != nil {
					return nil, false
				}
				rpsIdx = int(v)
			}
			if rpsIdx >= len(sps.rpsNumUsed) {
				return nil, false
			}
			numPocTotalCurr = sps.rpsNumUsed[rpsIdx]
		}
		if sps.longTermPresent {
			return nil, false // 長期参照つきは対象外(v1、まれ)
		}
		if sps.temporalMvp {
			b, err := r.u1() // slice_temporal_mvp_enabled
			if err != nil {
				return nil, false
			}
			sl.temporalMvp = b == 1
		}
	}
	if sps.sao {
		b, err := r.u1()
		if err != nil {
			return nil, false
		}
		sl.saoLuma = b == 1
		if sps.chromaFormatIDC != 0 {
			b, err := r.u1()
			if err != nil {
				return nil, false
			}
			sl.saoChroma = b == 1
		}
	}
	if sl.isInter() {
		// P/B スライス固有ブロック(7.3.6.1)
		sl.numRefIdx[0] = pps.numRefIdxL0Default
		if sl.isB() {
			sl.numRefIdx[1] = pps.numRefIdxL1Default
		}
		ov, err := r.u1() // num_ref_idx_active_override_flag
		if err != nil {
			return nil, false
		}
		if ov == 1 {
			v, err := r.ue()
			if err != nil || v > 14 {
				return nil, false
			}
			sl.numRefIdx[0] = int(v) + 1
			if sl.isB() {
				v, err := r.ue()
				if err != nil || v > 14 {
					return nil, false
				}
				sl.numRefIdx[1] = int(v) + 1
			}
		}
		if numPocTotalCurr == 0 {
			return nil, false // 参照ゼロの P/B は不正
		}
		// ref_pic_lists_modification
		if pps.listsModPresent && numPocTotalCurr > 1 {
			nb := ceilLog2(numPocTotalCurr)
			m0, err := r.u1()
			if err != nil {
				return nil, false
			}
			if m0 == 1 {
				for i := 0; i < sl.numRefIdx[0]; i++ {
					if _, err := r.u(nb); err != nil {
						return nil, false
					}
				}
			}
			if sl.isB() {
				m1, err := r.u1()
				if err != nil {
					return nil, false
				}
				if m1 == 1 {
					for i := 0; i < sl.numRefIdx[1]; i++ {
						if _, err := r.u(nb); err != nil {
							return nil, false
						}
					}
				}
			}
		}
		if sl.isB() {
			b, err := r.u1() // mvd_l1_zero_flag
			if err != nil {
				return nil, false
			}
			sl.mvdL1Zero = b == 1
		}
		if pps.cabacInitPresent {
			b, err := r.u1() // cabac_init_flag
			if err != nil {
				return nil, false
			}
			sl.cabacInitFlag = b == 1
		}
		if sl.temporalMvp {
			collocatedList := 0
			if sl.isB() {
				c, err := r.u1() // collocated_from_l0_flag
				if err != nil {
					return nil, false
				}
				if c == 0 {
					collocatedList = 1
				}
			}
			if sl.numRefIdx[collocatedList] > 1 {
				if _, err := r.ue(); err != nil { // collocated_ref_idx
					return nil, false
				}
			}
		}
		wp := (pps.weightedPred && sl.sliceType == 1) ||
			(pps.weightedBipred && sl.isB())
		if wp {
			if !hevcParsePredWeightTable(r, sps, sl) {
				return nil, false
			}
		}
		five, err := r.ue() // five_minus_max_num_merge_cand
		if err != nil || five > 4 {
			return nil, false
		}
		sl.maxNumMerge = 5 - int(five)
		if sl.maxNumMerge < 1 {
			return nil, false
		}
		// motion_vector_resolution_control_idc(SCC 拡張)は非対応前提で
		// use_integer_mv_flag は読まない(通常ストリームでは出ない)。
	}
	q, err := r.se()
	if err != nil {
		return nil, false
	}
	sl.sliceQP = pps.initQP + int(q)
	if pps.sliceChromaQPOffsets {
		if _, err := r.se(); err != nil {
			return nil, false
		}
		if _, err := r.se(); err != nil {
			return nil, false
		}
	}
	if pps.deblockingOverride {
		ov, err := r.u1()
		if err != nil {
			return nil, false
		}
		if ov == 1 {
			dis, err := r.u1()
			if err != nil {
				return nil, false
			}
			if dis == 0 {
				if _, err := r.se(); err != nil {
					return nil, false
				}
				if _, err := r.se(); err != nil {
					return nil, false
				}
			}
			if pps.loopFilterAcrossSlice && (sl.saoLuma || sl.saoChroma || dis == 0) {
				if _, err := r.u1(); err != nil {
					return nil, false
				}
			}
		} else if pps.loopFilterAcrossSlice && (sl.saoLuma || sl.saoChroma || !pps.deblockingDisabled) {
			if _, err := r.u1(); err != nil {
				return nil, false
			}
		}
	} else if pps.loopFilterAcrossSlice && (sl.saoLuma || sl.saoChroma || !pps.deblockingDisabled) {
		if _, err := r.u1(); err != nil {
			return nil, false
		}
	}
	if pps.tilesEnabled || pps.entropyCodingSync {
		n, err := r.ue()
		if err != nil || n > 1<<16 {
			return nil, false
		}
		sl.numEntry = int(n)
		if n > 0 {
			ol, err := r.ue()
			if err != nil || ol > 31 {
				return nil, false
			}
			for i := 0; i < int(n); i++ {
				v, err := r.u(int(ol) + 1)
				if err != nil {
					return nil, false
				}
				sl.entryOffsets = append(sl.entryOffsets, int(v)+1)
			}
		}
	}
	if pps.sliceHdrExtPresent {
		l, err := r.ue()
		if err != nil {
			return nil, false
		}
		for i := 0; i < int(l); i++ {
			if _, err := r.u(8); err != nil {
				return nil, false
			}
		}
	}
	// byte_alignment
	one, err := r.u1()
	if err != nil || one != 1 {
		return nil, false
	}
	for r.pos%8 != 0 {
		z, err := r.u1()
		if err != nil || z != 0 {
			return nil, false
		}
	}
	sl.headerBits = r.pos
	return sl, true
}

// hevcParsePredWeightTable は pred_weight_table(7.3.6.3)を読み飛ばす。
// 重み値は復号に不要(パースのみ)なので構造だけ追う。
func hevcParsePredWeightTable(r *h264Reader, sps *hevcSPS, sl *hevcSlice) bool {
	if _, err := r.ue(); err != nil { // luma_log2_weight_denom
		return false
	}
	if sps.chromaFormatIDC != 0 {
		if _, err := r.se(); err != nil { // delta_chroma_log2_weight_denom
			return false
		}
	}
	lists := 1
	if sl.isB() {
		lists = 2
	}
	for l := 0; l < lists; l++ {
		n := sl.numRefIdx[l]
		lumaFlags := make([]int, n)
		for i := 0; i < n; i++ {
			b, err := r.u1() // luma_weight_lX_flag
			if err != nil {
				return false
			}
			lumaFlags[i] = int(b)
		}
		chromaFlags := make([]int, n)
		if sps.chromaFormatIDC != 0 {
			for i := 0; i < n; i++ {
				b, err := r.u1() // chroma_weight_lX_flag
				if err != nil {
					return false
				}
				chromaFlags[i] = int(b)
			}
		}
		for i := 0; i < n; i++ {
			if lumaFlags[i] == 1 {
				if _, err := r.se(); err != nil { // delta_luma_weight
					return false
				}
				if _, err := r.se(); err != nil { // luma_offset
					return false
				}
			}
			if chromaFlags[i] == 1 {
				for j := 0; j < 2; j++ {
					if _, err := r.se(); err != nil { // delta_chroma_weight
						return false
					}
					if _, err := r.se(); err != nil { // delta_chroma_offset
						return false
					}
				}
			}
		}
	}
	return true
}

// hevcParseShortTermRPSSlice はスライスヘッダ内の st_ref_pic_set。
// スライス内では inter 予測時に delta_idx_minus1 が現れる。
// 戻り値: NumDeltaPocs, NumPocTotalCurr 寄与(used 数), ok。
func hevcParseShortTermRPSSlice(r *h264Reader, idx int, numDelta []int) (int, int, bool) {
	interPred := false
	if idx != 0 {
		b, err := r.u1()
		if err != nil {
			return 0, 0, false
		}
		interPred = b == 1
	}
	if interPred {
		// スライス内では delta_idx_minus1 が常に存在する
		d, err := r.ue()
		if err != nil {
			return 0, 0, false
		}
		refIdx := idx - 1 - int(d)
		if refIdx < 0 || refIdx >= len(numDelta) {
			return 0, 0, false
		}
		if _, err := r.u1(); err != nil {
			return 0, 0, false
		}
		if _, err := r.ue(); err != nil {
			return 0, 0, false
		}
		count, used := 0, 0
		for j := 0; j <= numDelta[refIdx]; j++ {
			u, err := r.u1()
			if err != nil {
				return 0, 0, false
			}
			useDelta := 1
			if u == 0 {
				ud, err := r.u1()
				if err != nil {
					return 0, 0, false
				}
				useDelta = int(ud)
			}
			if u == 1 {
				used++
			}
			if u == 1 || useDelta == 1 {
				count++
			}
		}
		return count, used, true
	}
	nNeg, err := r.ue()
	if err != nil || nNeg > 16 {
		return 0, 0, false
	}
	nPos, err := r.ue()
	if err != nil || nPos > 16 {
		return 0, 0, false
	}
	used := 0
	for i := 0; i < int(nNeg)+int(nPos); i++ {
		if _, err := r.ue(); err != nil {
			return 0, 0, false
		}
		u, err := r.u1()
		if err != nil {
			return 0, 0, false
		}
		if u == 1 {
			used++
		}
	}
	return int(nNeg) + int(nPos), used, true
}
