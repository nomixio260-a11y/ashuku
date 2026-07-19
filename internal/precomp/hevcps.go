package precomp

// HEVC SPS/PPS の解析(parse-only)。
//
// SPS/PPS NAL は骨格に原文保持するため再符号化は不要で、スライスヘッダ・
// CTU 解析に必要なフィールドが現れる位置まで**ビット厳密に**読めればよい
// (SPS は strong_intra_smoothing まで、PPS は slice_segment_header_
// extension_present まで。以降の VUI/拡張は読まない)。

// hevcSPS は SPS の解析結果(必要フィールドのみ)。
type hevcSPS struct {
	spsID            int
	chromaFormatIDC  int
	width, height    int
	bitDepth         int
	log2MaxPocLsb    int
	log2MinCb        int // log2_min_luma_coding_block_size
	log2CtbSize      int
	log2MinTb        int
	log2MaxTb        int
	maxTrDepthInter  int
	maxTrDepthIntra  int
	scalingList      bool
	amp              bool
	sao              bool
	pcmEnabled       bool
	pcmBitDepthLuma  int
	pcmBitDepthChr   int
	log2MinPcm       int
	log2MaxPcm       int
	pcmLoopFilterOff bool
	numDeltaPocs     []int // 各 st_ref_pic_set の NumDeltaPocs
	longTermPresent  bool
	numLongTermSPS   int
	temporalMvp      bool
	// 導出値
	ctbW, ctbH int // ピクチャ内 CTB 数
}

// hevcPPS は PPS の解析結果。
type hevcPPS struct {
	ppsID                 int
	spsID                 int
	dependentSlices       bool
	outputFlagPresent     bool
	numExtraSliceBits     int
	signDataHiding        bool
	cabacInitPresent      bool
	initQP                int
	constrainedIntraPred  bool
	transformSkip         bool
	cuQPDeltaEnabled      bool
	diffCuQPDeltaDepth    int
	cbQPOffset, crQPOffset int
	sliceChromaQPOffsets  bool
	weightedPred          bool
	weightedBipred        bool
	transquantBypass      bool
	tilesEnabled          bool
	entropyCodingSync     bool // WPP
	loopFilterAcrossSlice bool
	deblockingOverride    bool
	deblockingDisabled    bool
	betaOffset, tcOffset  int
	scalingListPresent    bool
	listsModPresent       bool
	log2ParallelMerge     int
	sliceHdrExtPresent    bool
}

// hevcParsePTL は profile_tier_level を読み飛ばす。
func hevcParsePTL(r *h264Reader, maxSubLayersMinus1 int) bool {
	// general PTL: 2+1+5+32+4+43+1 = 88 ビット + level 8 ビット
	if _, err := r.u(24); err != nil { // space/tier/profile_idc + compat[0:16]
		return false
	}
	if _, err := r.u(24); err != nil { // compat[16:32] + prog/inter/nonpacked/frameonly + reserved 頭
		return false
	}
	// ここまで 48。残り general 88-48=40 + level 8 = 48
	if _, err := r.u(40); err != nil {
		return false
	}
	if _, err := r.u(8); err != nil { // general_level_idc
		return false
	}
	if maxSubLayersMinus1 == 0 {
		return true
	}
	profPresent := make([]bool, maxSubLayersMinus1)
	lvlPresent := make([]bool, maxSubLayersMinus1)
	for i := 0; i < maxSubLayersMinus1; i++ {
		p, err := r.u1()
		if err != nil {
			return false
		}
		l, err := r.u1()
		if err != nil {
			return false
		}
		profPresent[i] = p == 1
		lvlPresent[i] = l == 1
	}
	for i := maxSubLayersMinus1; i < 8; i++ {
		if _, err := r.u(2); err != nil {
			return false
		}
	}
	for i := 0; i < maxSubLayersMinus1; i++ {
		if profPresent[i] {
			if _, err := r.u(64); err != nil {
				return false
			}
			if _, err := r.u(24); err != nil {
				return false
			}
		}
		if lvlPresent[i] {
			if _, err := r.u(8); err != nil {
				return false
			}
		}
	}
	return true
}

// hevcParseShortTermRPS は st_ref_pic_set を読み、NumDeltaPocs を返す。
// idx == 0 の場合 inter 予測フラグは存在しない。
func hevcParseShortTermRPS(r *h264Reader, idx int, prevNumDelta []int) (int, bool) {
	interPred := false
	if idx != 0 {
		b, err := r.u1()
		if err != nil {
			return 0, false
		}
		interPred = b == 1
	}
	if interPred {
		// SPS 内では RefRpsIdx = idx-1(delta_idx なし)
		if idx == 0 || idx-1 >= len(prevNumDelta) {
			return 0, false
		}
		if _, err := r.u1(); err != nil { // delta_rps_sign
			return 0, false
		}
		if _, err := r.ue(); err != nil { // abs_delta_rps_minus1
			return 0, false
		}
		refNum := prevNumDelta[idx-1]
		count := 0
		for j := 0; j <= refNum; j++ {
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

// hevcSkipScalingList は scaling_list_data を読み飛ばす。
func hevcSkipScalingList(r *h264Reader) bool {
	for sizeID := 0; sizeID < 4; sizeID++ {
		step := 1
		if sizeID == 3 {
			step = 3
		}
		for matrixID := 0; matrixID < 6; matrixID += step {
			pred, err := r.u1()
			if err != nil {
				return false
			}
			if pred == 0 {
				if _, err := r.ue(); err != nil { // pred_matrix_id_delta
					return false
				}
			} else {
				coefNum := 64
				if sizeID == 0 {
					coefNum = 16
				}
				if sizeID > 1 {
					if _, err := r.se(); err != nil { // dc coef
						return false
					}
				}
				for i := 0; i < coefNum; i++ {
					if _, err := r.se(); err != nil {
						return false
					}
				}
			}
		}
	}
	return true
}

// parseHEVCSPS は SPS RBSP(NAL ヘッダ 2 バイトの後)を解析する。
func parseHEVCSPS(rbsp []byte) (*hevcSPS, bool) {
	r := &h264Reader{b: rbsp}
	s := &hevcSPS{}
	if _, err := r.u(4); err != nil { // vps_id
		return nil, false
	}
	msl, err := r.u(3)
	if err != nil {
		return nil, false
	}
	if _, err := r.u1(); err != nil { // temporal_id_nesting
		return nil, false
	}
	if !hevcParsePTL(r, int(msl)) {
		return nil, false
	}
	v, err := r.ue()
	if err != nil || v > 15 {
		return nil, false
	}
	s.spsID = int(v)
	if v, err = r.ue(); err != nil {
		return nil, false
	}
	s.chromaFormatIDC = int(v)
	if s.chromaFormatIDC == 3 {
		if _, err := r.u1(); err != nil {
			return nil, false
		}
	}
	if v, err = r.ue(); err != nil {
		return nil, false
	}
	s.width = int(v)
	if v, err = r.ue(); err != nil {
		return nil, false
	}
	s.height = int(v)
	cw, err := r.u1()
	if err != nil {
		return nil, false
	}
	if cw == 1 {
		for i := 0; i < 4; i++ {
			if _, err := r.ue(); err != nil {
				return nil, false
			}
		}
	}
	if v, err = r.ue(); err != nil {
		return nil, false
	}
	s.bitDepth = int(v) + 8
	if _, err = r.ue(); err != nil { // bit_depth_chroma
		return nil, false
	}
	if v, err = r.ue(); err != nil || v > 12 {
		return nil, false
	}
	s.log2MaxPocLsb = int(v) + 4
	subOrder, err := r.u1()
	if err != nil {
		return nil, false
	}
	start := 0
	if subOrder == 0 {
		start = int(msl)
	}
	for i := start; i <= int(msl); i++ {
		for k := 0; k < 3; k++ {
			if _, err := r.ue(); err != nil {
				return nil, false
			}
		}
	}
	if v, err = r.ue(); err != nil {
		return nil, false
	}
	s.log2MinCb = int(v) + 3
	if v, err = r.ue(); err != nil {
		return nil, false
	}
	s.log2CtbSize = s.log2MinCb + int(v)
	if v, err = r.ue(); err != nil {
		return nil, false
	}
	s.log2MinTb = int(v) + 2
	if v, err = r.ue(); err != nil {
		return nil, false
	}
	s.log2MaxTb = s.log2MinTb + int(v)
	if v, err = r.ue(); err != nil {
		return nil, false
	}
	s.maxTrDepthInter = int(v)
	if v, err = r.ue(); err != nil {
		return nil, false
	}
	s.maxTrDepthIntra = int(v)
	sl, err := r.u1()
	if err != nil {
		return nil, false
	}
	s.scalingList = sl == 1
	if s.scalingList {
		present, err := r.u1()
		if err != nil {
			return nil, false
		}
		if present == 1 && !hevcSkipScalingList(r) {
			return nil, false
		}
	}
	b, err := r.u1()
	if err != nil {
		return nil, false
	}
	s.amp = b == 1
	if b, err = r.u1(); err != nil {
		return nil, false
	}
	s.sao = b == 1
	if b, err = r.u1(); err != nil {
		return nil, false
	}
	s.pcmEnabled = b == 1
	if s.pcmEnabled {
		v4, err := r.u(4)
		if err != nil {
			return nil, false
		}
		s.pcmBitDepthLuma = int(v4) + 1
		if v4, err = r.u(4); err != nil {
			return nil, false
		}
		s.pcmBitDepthChr = int(v4) + 1
		if v, err = r.ue(); err != nil {
			return nil, false
		}
		s.log2MinPcm = int(v) + 3
		if v, err = r.ue(); err != nil {
			return nil, false
		}
		s.log2MaxPcm = s.log2MinPcm + int(v)
		if b, err = r.u1(); err != nil {
			return nil, false
		}
		s.pcmLoopFilterOff = b == 1
	}
	nRps, err := r.ue()
	if err != nil || nRps > 64 {
		return nil, false
	}
	for i := 0; i < int(nRps); i++ {
		nd, ok := hevcParseShortTermRPS(r, i, s.numDeltaPocs)
		if !ok {
			return nil, false
		}
		s.numDeltaPocs = append(s.numDeltaPocs, nd)
	}
	if b, err = r.u1(); err != nil {
		return nil, false
	}
	s.longTermPresent = b == 1
	if s.longTermPresent {
		n, err := r.ue()
		if err != nil || n > 32 {
			return nil, false
		}
		s.numLongTermSPS = int(n)
		for i := 0; i < int(n); i++ {
			if _, err := r.u(s.log2MaxPocLsb); err != nil {
				return nil, false
			}
			if _, err := r.u1(); err != nil {
				return nil, false
			}
		}
	}
	if b, err = r.u1(); err != nil {
		return nil, false
	}
	s.temporalMvp = b == 1
	if _, err = r.u1(); err != nil { // strong_intra_smoothing
		return nil, false
	}
	// 以降(VUI/拡張)はスライス解析に不要。骨格に原文保持されるため読まない。
	ctb := 1 << uint(s.log2CtbSize)
	s.ctbW = (s.width + ctb - 1) / ctb
	s.ctbH = (s.height + ctb - 1) / ctb
	if s.ctbW <= 0 || s.ctbH <= 0 {
		return nil, false
	}
	return s, true
}

// parseHEVCPPS は PPS RBSP を解析する。
func parseHEVCPPS(rbsp []byte) (*hevcPPS, bool) {
	r := &h264Reader{b: rbsp}
	p := &hevcPPS{}
	v, err := r.ue()
	if err != nil || v > 63 {
		return nil, false
	}
	p.ppsID = int(v)
	if v, err = r.ue(); err != nil || v > 15 {
		return nil, false
	}
	p.spsID = int(v)
	b, err := r.u1()
	if err != nil {
		return nil, false
	}
	p.dependentSlices = b == 1
	if b, err = r.u1(); err != nil {
		return nil, false
	}
	p.outputFlagPresent = b == 1
	v3, err := r.u(3)
	if err != nil {
		return nil, false
	}
	p.numExtraSliceBits = int(v3)
	if b, err = r.u1(); err != nil {
		return nil, false
	}
	p.signDataHiding = b == 1
	if b, err = r.u1(); err != nil {
		return nil, false
	}
	p.cabacInitPresent = b == 1
	if _, err = r.ue(); err != nil { // num_ref_idx_l0_default_active_minus1
		return nil, false
	}
	if _, err = r.ue(); err != nil { // num_ref_idx_l1_default_active_minus1
		return nil, false
	}
	sv, err := r.se()
	if err != nil {
		return nil, false
	}
	p.initQP = 26 + int(sv)
	if b, err = r.u1(); err != nil {
		return nil, false
	}
	p.constrainedIntraPred = b == 1
	if b, err = r.u1(); err != nil {
		return nil, false
	}
	p.transformSkip = b == 1
	if b, err = r.u1(); err != nil {
		return nil, false
	}
	p.cuQPDeltaEnabled = b == 1
	if p.cuQPDeltaEnabled {
		if v, err = r.ue(); err != nil {
			return nil, false
		}
		p.diffCuQPDeltaDepth = int(v)
	}
	if sv, err = r.se(); err != nil {
		return nil, false
	}
	p.cbQPOffset = int(sv)
	if sv, err = r.se(); err != nil {
		return nil, false
	}
	p.crQPOffset = int(sv)
	if b, err = r.u1(); err != nil {
		return nil, false
	}
	p.sliceChromaQPOffsets = b == 1
	if b, err = r.u1(); err != nil {
		return nil, false
	}
	p.weightedPred = b == 1
	if b, err = r.u1(); err != nil {
		return nil, false
	}
	p.weightedBipred = b == 1
	if b, err = r.u1(); err != nil {
		return nil, false
	}
	p.transquantBypass = b == 1
	if b, err = r.u1(); err != nil {
		return nil, false
	}
	p.tilesEnabled = b == 1
	if b, err = r.u1(); err != nil {
		return nil, false
	}
	p.entropyCodingSync = b == 1
	if p.tilesEnabled {
		return nil, false // タイルは対象外(v1)
	}
	if b, err = r.u1(); err != nil {
		return nil, false
	}
	p.loopFilterAcrossSlice = b == 1
	db, err := r.u1() // deblocking_filter_control_present
	if err != nil {
		return nil, false
	}
	if db == 1 {
		if b, err = r.u1(); err != nil {
			return nil, false
		}
		p.deblockingOverride = b == 1
		if b, err = r.u1(); err != nil {
			return nil, false
		}
		p.deblockingDisabled = b == 1
		if !p.deblockingDisabled {
			if sv, err = r.se(); err != nil {
				return nil, false
			}
			p.betaOffset = int(sv) * 2
			if sv, err = r.se(); err != nil {
				return nil, false
			}
			p.tcOffset = int(sv) * 2
		}
	}
	if b, err = r.u1(); err != nil {
		return nil, false
	}
	p.scalingListPresent = b == 1
	if p.scalingListPresent && !hevcSkipScalingList(r) {
		return nil, false
	}
	if b, err = r.u1(); err != nil {
		return nil, false
	}
	p.listsModPresent = b == 1
	if v, err = r.ue(); err != nil {
		return nil, false
	}
	p.log2ParallelMerge = int(v) + 2
	if b, err = r.u1(); err != nil {
		return nil, false
	}
	p.sliceHdrExtPresent = b == 1
	// 以降(pps_extension)はスライス解析に不要。
	return p, true
}
