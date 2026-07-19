package precomp

// H.264 パラメータセット(SPS/PPS)とスライスヘッダの解析。
//
// SPS/PPS/SEI などの NAL は**原文のまま**スケルトンとして保存する(再符号化
// しない)。ここでの解析は、スライスデータ(マクロブロック層)を読むのに
// 必要なパラメータを得るためだけのもの。対象外の機能(CABAC・フィールド・
// FMO・SP/SI など)を見つけたら即 false を返し、呼び出し側が安全に素通しする。

import "errors"

var errH264Unsupported = errors.New("h264: 対象外の構文")

type h264SPS struct {
	profileIDC       int
	chromaFormatIDC  int // 1=4:2:0 のみ対応
	log2MaxFrameNum  int
	picOrderCntType  int
	log2MaxPocLsb    int
	deltaPicAlways0  bool
	maxNumRefFrames  int
	picWidthInMbs    int
	picHeightInMbs   int
	frameMbsOnly     bool
	separateColour   bool
	direct8x8        bool
	qpprimeYZeroFlag bool
}

func parseSPS(rbsp []byte) (*h264SPS, bool) {
	r := &h264Reader{b: rbsp}
	// NAL ヘッダ(1バイト)を飛ばした RBSP が渡される前提
	s := &h264SPS{chromaFormatIDC: 1}
	prof, err := r.u(8)
	if err != nil {
		return nil, false
	}
	s.profileIDC = int(prof)
	if _, err := r.u(8); err != nil { // constraint flags + reserved
		return nil, false
	}
	if _, err := r.u(8); err != nil { // level_idc
		return nil, false
	}
	if _, err := r.ue(); err != nil { // seq_parameter_set_id
		return nil, false
	}
	switch s.profileIDC {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		cf, err := r.ue()
		if err != nil {
			return nil, false
		}
		s.chromaFormatIDC = int(cf)
		if cf == 3 {
			b, err := r.u1()
			if err != nil {
				return nil, false
			}
			s.separateColour = b == 1
		}
		if _, err := r.ue(); err != nil { // bit_depth_luma_minus8
			return nil, false
		}
		if _, err := r.ue(); err != nil { // bit_depth_chroma_minus8
			return nil, false
		}
		b, err := r.u1() // qpprime_y_zero_transform_bypass
		if err != nil {
			return nil, false
		}
		s.qpprimeYZeroFlag = b == 1
		sm, err := r.u1() // seq_scaling_matrix_present
		if err != nil {
			return nil, false
		}
		if sm == 1 {
			return nil, false // スケーリングリストは対象外(High 系)
		}
	}
	v, err := r.ue()
	if err != nil || v > 12 { // 規格上 log2_max_frame_num_minus4 ≤ 12
		return nil, false
	}
	s.log2MaxFrameNum = int(v) + 4
	poc, err := r.ue()
	if err != nil {
		return nil, false
	}
	s.picOrderCntType = int(poc)
	switch poc {
	case 0:
		v, err := r.ue()
		if err != nil || v > 12 {
			return nil, false
		}
		s.log2MaxPocLsb = int(v) + 4
	case 1:
		b, err := r.u1()
		if err != nil {
			return nil, false
		}
		s.deltaPicAlways0 = b == 1
		if _, err := r.se(); err != nil {
			return nil, false
		}
		if _, err := r.se(); err != nil {
			return nil, false
		}
		n, err := r.ue()
		if err != nil {
			return nil, false
		}
		for i := uint32(0); i < n; i++ {
			if _, err := r.se(); err != nil {
				return nil, false
			}
		}
	}
	v, err = r.ue()
	if err != nil {
		return nil, false
	}
	s.maxNumRefFrames = int(v)
	if _, err := r.u1(); err != nil { // gaps_in_frame_num_value_allowed
		return nil, false
	}
	v, err = r.ue()
	if err != nil || v >= 1024 { // 16K 画素相当まで(異常値の巨大確保を防ぐ)
		return nil, false
	}
	s.picWidthInMbs = int(v) + 1
	v, err = r.ue()
	if err != nil || v >= 1024 {
		return nil, false
	}
	s.picHeightInMbs = int(v) + 1
	if s.picWidthInMbs*s.picHeightInMbs > 1<<18 {
		return nil, false
	}
	fm, err := r.u1()
	if err != nil {
		return nil, false
	}
	s.frameMbsOnly = fm == 1
	if !s.frameMbsOnly {
		return nil, false // インターレースは対象外
	}
	if s.chromaFormatIDC != 1 || s.separateColour {
		return nil, false // 4:2:0 のみ
	}
	// 以降(direct_8x8, cropping, VUI)はスライス解析に不要
	return s, true
}

type h264PPS struct {
	spsID                  int
	entropyCodingMode      bool // true=CABAC(対象外)
	bottomFieldPicOrder    bool
	numSliceGroups         int
	numRefIdxL0Default     int
	numRefIdxL1Default     int
	weightedPred           bool
	weightedBipredIDC      int
	picInitQP              int
	chromaQPIndexOffset    int
	deblockingControlPres  bool
	constrainedIntraPred   bool
	redundantPicCntPresent bool
	transform8x8           bool
}

func parsePPS(rbsp []byte) (*h264PPS, bool) {
	r := &h264Reader{b: rbsp}
	p := &h264PPS{}
	if _, err := r.ue(); err != nil { // pps_id
		return nil, false
	}
	v, err := r.ue()
	if err != nil {
		return nil, false
	}
	p.spsID = int(v)
	b, err := r.u1()
	if err != nil {
		return nil, false
	}
	p.entropyCodingMode = b == 1
	b, err = r.u1()
	if err != nil {
		return nil, false
	}
	p.bottomFieldPicOrder = b == 1
	v, err = r.ue()
	if err != nil {
		return nil, false
	}
	p.numSliceGroups = int(v) + 1
	if p.numSliceGroups > 1 {
		return nil, false // FMO は対象外
	}
	v, err = r.ue()
	if err != nil {
		return nil, false
	}
	p.numRefIdxL0Default = int(v) + 1
	v, err = r.ue()
	if err != nil {
		return nil, false
	}
	p.numRefIdxL1Default = int(v) + 1
	b, err = r.u1()
	if err != nil {
		return nil, false
	}
	p.weightedPred = b == 1
	w2, err := r.u(2)
	if err != nil {
		return nil, false
	}
	p.weightedBipredIDC = int(w2)
	q, err := r.se()
	if err != nil {
		return nil, false
	}
	p.picInitQP = 26 + int(q)
	if _, err := r.se(); err != nil { // pic_init_qs
		return nil, false
	}
	if _, err := r.se(); err != nil { // chroma_qp_index_offset
		return nil, false
	}
	b, err = r.u1()
	if err != nil {
		return nil, false
	}
	p.deblockingControlPres = b == 1
	b, err = r.u1()
	if err != nil {
		return nil, false
	}
	p.constrainedIntraPred = b == 1
	b, err = r.u1()
	if err != nil {
		return nil, false
	}
	p.redundantPicCntPresent = b == 1
	if r.moreRBSPData() {
		// transform_8x8_mode / scaling matrix / second_chroma_qp — High 系。
		// transform 8x8 は CAVLC 解析に影響するため対象外にする。
		return nil, false
	}
	return p, true
}

// h264Slice はスライスヘッダの解析結果(ヘッダ原文ビットは別途保持)。
type h264Slice struct {
	firstMB       int
	sliceType     int // 0/5=P, 2/7=I のみ対応
	numRefIdxL0   int
	headerBits    int // RBSP 先頭(NALヘッダ後)からヘッダ末尾までのビット数
	sliceQP       int
	nalRefIDC     int
	idr           bool
	disableDeblk  int
	cabacInitPres bool
	cabacInitIDC  int
}

// parseSliceHeader はスライスヘッダを解析し、データ開始ビット位置を得る。
// r は RBSP(NAL ヘッダの直後)を指す。
func parseSliceHeader(r *h264Reader, sps *h264SPS, pps *h264PPS, nalType, nalRefIDC int) (*h264Slice, bool) {
	sl := &h264Slice{idr: nalType == 5, nalRefIDC: nalRefIDC}
	v, err := r.ue()
	if err != nil {
		return nil, false
	}
	sl.firstMB = int(v)
	st, err := r.ue()
	if err != nil {
		return nil, false
	}
	sl.sliceType = int(st)
	switch sl.sliceType {
	case 0, 5: // P
	case 2, 7: // I
	default:
		return nil, false // B/SP/SI は対象外
	}
	if _, err := r.ue(); err != nil { // pps_id(呼び出し側で選択済み)
		return nil, false
	}
	if _, err := r.u(sps.log2MaxFrameNum); err != nil { // frame_num
		return nil, false
	}
	// frame_mbs_only=1 なので field フラグなし
	if sl.idr {
		if _, err := r.ue(); err != nil { // idr_pic_id
			return nil, false
		}
	}
	switch sps.picOrderCntType {
	case 0:
		if _, err := r.u(sps.log2MaxPocLsb); err != nil {
			return nil, false
		}
		if pps.bottomFieldPicOrder {
			if _, err := r.se(); err != nil {
				return nil, false
			}
		}
	case 1:
		if !sps.deltaPicAlways0 {
			if _, err := r.se(); err != nil {
				return nil, false
			}
			if pps.bottomFieldPicOrder {
				if _, err := r.se(); err != nil {
					return nil, false
				}
			}
		}
	}
	if pps.redundantPicCntPresent {
		if _, err := r.ue(); err != nil {
			return nil, false
		}
	}
	sl.numRefIdxL0 = pps.numRefIdxL0Default
	isP := sl.sliceType == 0 || sl.sliceType == 5
	if isP {
		b, err := r.u1() // num_ref_idx_active_override
		if err != nil {
			return nil, false
		}
		if b == 1 {
			v, err := r.ue()
			if err != nil {
				return nil, false
			}
			sl.numRefIdxL0 = int(v) + 1
		}
		// ref_pic_list_modification
		b, err = r.u1()
		if err != nil {
			return nil, false
		}
		if b == 1 {
			for {
				op, err := r.ue()
				if err != nil {
					return nil, false
				}
				if op == 3 {
					break
				}
				if op > 3 {
					return nil, false
				}
				if _, err := r.ue(); err != nil {
					return nil, false
				}
			}
		}
	}
	if pps.weightedPred && isP {
		return nil, false // 重み付き予測テーブルは対象外(baseline では出ない)
	}
	if sl.nalRefIDC != 0 {
		// dec_ref_pic_marking
		if sl.idr {
			if _, err := r.u(2); err != nil { // no_output / long_term_reference
				return nil, false
			}
		} else {
			b, err := r.u1() // adaptive_ref_pic_marking_mode
			if err != nil {
				return nil, false
			}
			if b == 1 {
				for {
					op, err := r.ue()
					if err != nil {
						return nil, false
					}
					if op == 0 {
						break
					}
					switch op {
					case 1, 3, 4, 6:
						if _, err := r.ue(); err != nil {
							return nil, false
						}
						if op == 3 {
							if _, err := r.ue(); err != nil {
								return nil, false
							}
						}
					case 2, 5:
						if op == 2 {
							if _, err := r.ue(); err != nil {
								return nil, false
							}
						}
					default:
						return nil, false
					}
				}
			}
		}
	}
	// CABAC はスライスヘッダ後に cabac_init_idc(ue)が続く。ここでは
	// エントロピーモードで分岐して読み進める(CAVLC 経路は entropyCodingMode=false)。
	if pps.entropyCodingMode && sl.sliceType != 2 && sl.sliceType != 7 {
		v, err := r.ue() // cabac_init_idc(I 以外)
		if err != nil || v > 2 {
			return nil, false
		}
		sl.cabacInitIDC = int(v)
	}
	q, err := r.se()
	if err != nil {
		return nil, false
	}
	sl.sliceQP = pps.picInitQP + int(q)
	if pps.deblockingControlPres {
		d, err := r.ue()
		if err != nil {
			return nil, false
		}
		sl.disableDeblk = int(d)
		if d != 1 {
			if _, err := r.se(); err != nil {
				return nil, false
			}
			if _, err := r.se(); err != nil {
				return nil, false
			}
		}
	}
	// num_slice_groups==1 なので slice_group_change_cycle なし
	sl.headerBits = r.pos
	return sl, true
}
