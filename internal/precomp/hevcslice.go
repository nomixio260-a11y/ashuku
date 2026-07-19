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
}

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
			if _, ok := hevcParseShortTermRPSSlice(r, idx, sps.numDeltaPocs); !ok {
				return nil, false
			}
		} else if len(sps.numDeltaPocs) > 0 {
			nb := ceilLog2(len(sps.numDeltaPocs))
			if nb > 0 {
				if _, err := r.u(nb); err != nil {
					return nil, false
				}
			}
		}
		if sps.longTermPresent {
			return nil, false // 長期参照つきは対象外(v1、全イントラでは出ない)
		}
		if sps.temporalMvp {
			if _, err := r.u1(); err != nil { // slice_temporal_mvp_enabled
				return nil, false
			}
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
	if sl.sliceType != 2 {
		return nil, false // P/B スライスは対象外(v1: 全イントラのみ)
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

// hevcParseShortTermRPSSlice はスライスヘッダ内の st_ref_pic_set。
// スライス内では inter 予測時に delta_idx_minus1 が現れる。
func hevcParseShortTermRPSSlice(r *h264Reader, idx int, numDelta []int) (int, bool) {
	interPred := false
	if idx != 0 {
		b, err := r.u1()
		if err != nil {
			return 0, false
		}
		interPred = b == 1
	}
	if interPred {
		// スライス内では delta_idx_minus1 が常に存在する
		d, err := r.ue()
		if err != nil {
			return 0, false
		}
		refIdx := idx - 1 - int(d)
		if refIdx < 0 || refIdx >= len(numDelta) {
			return 0, false
		}
		if _, err := r.u1(); err != nil {
			return 0, false
		}
		if _, err := r.ue(); err != nil {
			return 0, false
		}
		count := 0
		for j := 0; j <= numDelta[refIdx]; j++ {
			used, err := r.u1()
			if err != nil {
				return 0, false
			}
			useDelta := 1
			if used == 0 {
				ud, err := r.u1()
				if err != nil {
					return 0, false
				}
				useDelta = int(ud)
			}
			if used == 1 || useDelta == 1 {
				count++
			}
		}
		return count, true
	}
	nNeg, err := r.ue()
	if err != nil || nNeg > 16 {
		return 0, false
	}
	nPos, err := r.ue()
	if err != nil || nPos > 16 {
		return 0, false
	}
	for i := 0; i < int(nNeg)+int(nPos); i++ {
		if _, err := r.ue(); err != nil {
			return 0, false
		}
		if _, err := r.u1(); err != nil {
			return 0, false
		}
	}
	return int(nNeg) + int(nPos), true
}
