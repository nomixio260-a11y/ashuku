package precomp

// HEVC CABAC 文脈レイアウトと初期化(ITU-T H.265 9.3.2.2)。
// 算術エンジン自体(decodeDecision/Bypass/Terminate と LPS・遷移テーブル)は
// H.264 と完全に同一なので cabacengine.go を共用する。文脈は 199 スロットの
// 一次元配列で持ち、各シンタックス要素の基底オフセットを定数で引く。

// 文脈基底オフセット(hevcCtxInit の並び順に一致)。
const (
	hevcCtxSaoMerge      = 0   // sao_merge_flag (1)
	hevcCtxSaoTypeIdx    = 1   // sao_type_idx (1)
	hevcCtxSplitCU       = 2   // split_cu_flag (3)
	hevcCtxTQBypass      = 5   // cu_transquant_bypass_flag (1)
	hevcCtxSkipFlag      = 6   // cu_skip_flag (3)
	hevcCtxQPDelta       = 9   // cu_qp_delta_abs (3)
	hevcCtxPredMode      = 12  // pred_mode_flag (1)
	hevcCtxPartMode      = 13  // part_mode (4)
	hevcCtxPrevIntraLuma = 17  // prev_intra_luma_pred_flag (1)
	hevcCtxIntraChroma   = 18  // intra_chroma_pred_mode (2)
	hevcCtxMergeFlag     = 20  // merge_flag (1)
	hevcCtxMergeIdx      = 21  // merge_idx (1 ctx + bypass)
	hevcCtxInterPredIdc  = 22  // inter_pred_idc (5)
	hevcCtxRefIdx        = 27  // ref_idx_lX (L0/L1 共用、先頭2 bin が ctx)
	hevcCtxAbsMvdGt0     = 31  // abs_mvd_greater0_flag(x/y 共用)
	hevcCtxAbsMvdGt1     = 33  // abs_mvd_greater1_flag(実際は +1 = 34、x/y 共用)
	hevcCtxMvpFlag       = 35  // mvp_lX_flag (1)
	hevcCtxNoResidual    = 36  // rqt_root_cbf (1)
	hevcCtxSplitTrafo    = 37  // split_transform_flag (3)
	hevcCtxCbfLuma       = 40  // cbf_luma (2)
	hevcCtxCbfCbCr       = 42  // cbf_cb / cbf_cr (5)
	hevcCtxTransformSkip = 47  // transform_skip_flag (2: luma, chroma)
	hevcCtxLastXPrefix   = 53  // last_sig_coeff_x_prefix (18)
	hevcCtxLastYPrefix   = 71  // last_sig_coeff_y_prefix (18)
	hevcCtxSigGroup      = 89  // coded_sub_block_flag (4)
	hevcCtxSigCoeff      = 93  // sig_coeff_flag (44)
	hevcCtxGreater1      = 137 // coeff_abs_level_greater1_flag (24)
	hevcCtxGreater2      = 161 // coeff_abs_level_greater2_flag (6)
)

// hevcInitType はスライスタイプ(0=B 1=P 2=I)と cabac_init_flag から
// 初期化テーブル行を選ぶ。I→0、P→1、B→2、init_flag で P/B 反転。
func hevcInitType(sliceType int, cabacInitFlag bool) int {
	t := 2 - sliceType
	if cabacInitFlag && sliceType != 2 {
		t ^= 3
	}
	return t
}

// hevcInitStates は全 199 文脈をスライス QP で初期化する。生成される
// 状態バイトは H.264 と同じ s7 = 2σ + valMPS 表現。
func hevcInitStates(states *[hevcNumContexts]uint8, sliceQP, initType int) {
	qp := sliceQP
	if qp < 0 {
		qp = 0
	}
	if qp > 51 {
		qp = 51
	}
	tab := &hevcCtxInit[initType]
	for i := 0; i < hevcNumContexts; i++ {
		iv := int(tab[i])
		m := (iv>>4)*5 - 45
		n := (iv&15)<<3 - 16
		pre := 2*((m*qp)>>4+n) - 127
		pre ^= pre >> 31 // 符号折返し(絶対値相当、二の補数)
		if pre > 124 {
			pre = 124 + (pre & 1)
		}
		states[i] = uint8(pre)
	}
}
