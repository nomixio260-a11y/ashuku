package precomp

// hevcNumContexts mirrors HEVC_CONTEXTS (FFmpeg libavcodec/hevc/hevcdec.h:51).
// Only the first 179 entries carry real init values (the 35 syntax elements
// enumerated in CABAC_ELEMS, cabac.c:35-84); indices 179..198 are the C
// zero-fill tail of the [3][HEVC_CONTEXTS] initializer (cabac.c:100).
const hevcNumContexts = 199

// hevcCtxInit is indexed [initType][ctxIdx]. initType = 2 - sliceType,
// XOR 3 when cabac_init_flag set on a non-I slice (cabac.c:432-436).
// Source: init_values[3][HEVC_CONTEXTS], cabac.c:100-332 (CNU = 154).
var hevcCtxInit = [3][hevcNumContexts]uint8{
	{ // init_type 0
		// sao_merge_flag (count 1)
		153,
		// sao_type_idx (count 1)
		200,
		// split_coding_unit_flag (count 3)
		139, 141, 157,
		// cu_transquant_bypass_flag (count 1)
		154,
		// skip_flag (count 3)
		154, 154, 154,
		// cu_qp_delta (count 3)
		154, 154, 154,
		// pred_mode (count 1)
		154,
		// part_mode (count 4)
		184, 154, 154, 154,
		// prev_intra_luma_pred_mode (count 1)
		184,
		// intra_chroma_pred_mode (count 2)
		63, 139,
		// merge_flag (count 1)
		154,
		// merge_idx (count 1)
		154,
		// inter_pred_idc (count 5)
		154, 154, 154, 154, 154,
		// ref_idx_l0 (count 2)
		154, 154,
		// ref_idx_l1 (count 2)
		154, 154,
		// abs_mvd_greater0_flag (count 2)
		154, 154,
		// abs_mvd_greater1_flag (count 2)
		154, 154,
		// mvp_lx_flag (count 1)
		154,
		// no_residual_data_flag (count 1)
		154,
		// split_transform_flag (count 3)
		153, 138, 138,
		// cbf_luma (count 2)
		111, 141,
		// cbf_cb_cr (count 5)
		94, 138, 182, 154, 154,
		// transform_skip_flag (count 2)
		139, 139,
		// explicit_rdpcm_flag (count 2)
		139, 139,
		// explicit_rdpcm_dir_flag (count 2)
		139, 139,
		// last_significant_coeff_x_prefix (count 18)
		110, 110, 124, 125, 140, 153, 125, 127, 140, 109, 111, 143, 127, 111, 79, 108, 123, 63,
		// last_significant_coeff_y_prefix (count 18)
		110, 110, 124, 125, 140, 153, 125, 127, 140, 109, 111, 143, 127, 111, 79, 108, 123, 63,
		// significant_coeff_group_flag (count 4)
		91, 171, 134, 141,
		// significant_coeff_flag (count 44)
		111, 111, 125, 110, 110, 94, 124, 108, 124, 107, 125, 141, 179, 153, 125, 107, 125, 141, 179,
		153, 125, 107, 125, 141, 179, 153, 125, 140, 139, 182, 182, 152, 136, 152, 136, 153, 136,
		139, 111, 136, 139, 111, 141, 111,
		// coeff_abs_level_greater1_flag (count 24)
		140, 92, 137, 138, 140, 152, 138, 139, 153, 74, 149, 92, 139, 107, 122, 152, 140, 179, 166,
		182, 140, 227, 122, 197,
		// coeff_abs_level_greater2_flag (count 6)
		138, 153, 136, 167, 152, 152,
		// log2_res_scale_abs (count 8)
		154, 154, 154, 154, 154, 154, 154, 154,
		// res_scale_sign_flag (count 2)
		154, 154,
		// cu_chroma_qp_offset_flag (count 1)
		154,
		// cu_chroma_qp_offset_idx (count 1)
		154,
		// zero-fill tail: ctxIdx 179..198 (unused; implicit 0 in the C initializer)
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	},
	{ // init_type 1
		// sao_merge_flag (count 1)
		153,
		// sao_type_idx (count 1)
		185,
		// split_coding_unit_flag (count 3)
		107, 139, 126,
		// cu_transquant_bypass_flag (count 1)
		154,
		// skip_flag (count 3)
		197, 185, 201,
		// cu_qp_delta (count 3)
		154, 154, 154,
		// pred_mode (count 1)
		149,
		// part_mode (count 4)
		154, 139, 154, 154,
		// prev_intra_luma_pred_mode (count 1)
		154,
		// intra_chroma_pred_mode (count 2)
		152, 139,
		// merge_flag (count 1)
		110,
		// merge_idx (count 1)
		122,
		// inter_pred_idc (count 5)
		95, 79, 63, 31, 31,
		// ref_idx_l0 (count 2)
		153, 153,
		// ref_idx_l1 (count 2)
		153, 153,
		// abs_mvd_greater0_flag (count 2)
		140, 198,
		// abs_mvd_greater1_flag (count 2)
		140, 198,
		// mvp_lx_flag (count 1)
		168,
		// no_residual_data_flag (count 1)
		79,
		// split_transform_flag (count 3)
		124, 138, 94,
		// cbf_luma (count 2)
		153, 111,
		// cbf_cb_cr (count 5)
		149, 107, 167, 154, 154,
		// transform_skip_flag (count 2)
		139, 139,
		// explicit_rdpcm_flag (count 2)
		139, 139,
		// explicit_rdpcm_dir_flag (count 2)
		139, 139,
		// last_significant_coeff_x_prefix (count 18)
		125, 110, 94, 110, 95, 79, 125, 111, 110, 78, 110, 111, 111, 95, 94, 108, 123, 108,
		// last_significant_coeff_y_prefix (count 18)
		125, 110, 94, 110, 95, 79, 125, 111, 110, 78, 110, 111, 111, 95, 94, 108, 123, 108,
		// significant_coeff_group_flag (count 4)
		121, 140, 61, 154,
		// significant_coeff_flag (count 44)
		155, 154, 139, 153, 139, 123, 123, 63, 153, 166, 183, 140, 136, 153, 154, 166, 183, 140, 136,
		153, 154, 166, 183, 140, 136, 153, 154, 170, 153, 123, 123, 107, 121, 107, 121, 167, 151,
		183, 140, 151, 183, 140, 140, 140,
		// coeff_abs_level_greater1_flag (count 24)
		154, 196, 196, 167, 154, 152, 167, 182, 182, 134, 149, 136, 153, 121, 136, 137, 169, 194,
		166, 167, 154, 167, 137, 182,
		// coeff_abs_level_greater2_flag (count 6)
		107, 167, 91, 122, 107, 167,
		// log2_res_scale_abs (count 8)
		154, 154, 154, 154, 154, 154, 154, 154,
		// res_scale_sign_flag (count 2)
		154, 154,
		// cu_chroma_qp_offset_flag (count 1)
		154,
		// cu_chroma_qp_offset_idx (count 1)
		154,
		// zero-fill tail: ctxIdx 179..198 (unused; implicit 0 in the C initializer)
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	},
	{ // init_type 2
		// sao_merge_flag (count 1)
		153,
		// sao_type_idx (count 1)
		160,
		// split_coding_unit_flag (count 3)
		107, 139, 126,
		// cu_transquant_bypass_flag (count 1)
		154,
		// skip_flag (count 3)
		197, 185, 201,
		// cu_qp_delta (count 3)
		154, 154, 154,
		// pred_mode (count 1)
		134,
		// part_mode (count 4)
		154, 139, 154, 154,
		// prev_intra_luma_pred_mode (count 1)
		183,
		// intra_chroma_pred_mode (count 2)
		152, 139,
		// merge_flag (count 1)
		154,
		// merge_idx (count 1)
		137,
		// inter_pred_idc (count 5)
		95, 79, 63, 31, 31,
		// ref_idx_l0 (count 2)
		153, 153,
		// ref_idx_l1 (count 2)
		153, 153,
		// abs_mvd_greater0_flag (count 2)
		169, 198,
		// abs_mvd_greater1_flag (count 2)
		169, 198,
		// mvp_lx_flag (count 1)
		168,
		// no_residual_data_flag (count 1)
		79,
		// split_transform_flag (count 3)
		224, 167, 122,
		// cbf_luma (count 2)
		153, 111,
		// cbf_cb_cr (count 5)
		149, 92, 167, 154, 154,
		// transform_skip_flag (count 2)
		139, 139,
		// explicit_rdpcm_flag (count 2)
		139, 139,
		// explicit_rdpcm_dir_flag (count 2)
		139, 139,
		// last_significant_coeff_x_prefix (count 18)
		125, 110, 124, 110, 95, 94, 125, 111, 111, 79, 125, 126, 111, 111, 79, 108, 123, 93,
		// last_significant_coeff_y_prefix (count 18)
		125, 110, 124, 110, 95, 94, 125, 111, 111, 79, 125, 126, 111, 111, 79, 108, 123, 93,
		// significant_coeff_group_flag (count 4)
		121, 140, 61, 154,
		// significant_coeff_flag (count 44)
		170, 154, 139, 153, 139, 123, 123, 63, 124, 166, 183, 140, 136, 153, 154, 166, 183, 140, 136,
		153, 154, 166, 183, 140, 136, 153, 154, 170, 153, 138, 138, 122, 121, 122, 121, 167, 151,
		183, 140, 151, 183, 140, 140, 140,
		// coeff_abs_level_greater1_flag (count 24)
		154, 196, 167, 167, 154, 152, 167, 182, 182, 134, 149, 136, 153, 121, 136, 122, 169, 208,
		166, 167, 154, 152, 167, 182,
		// coeff_abs_level_greater2_flag (count 6)
		107, 167, 91, 107, 107, 167,
		// log2_res_scale_abs (count 8)
		154, 154, 154, 154, 154, 154, 154, 154,
		// res_scale_sign_flag (count 2)
		154, 154,
		// cu_chroma_qp_offset_flag (count 1)
		154,
		// cu_chroma_qp_offset_idx (count 1)
		154,
		// zero-fill tail: ctxIdx 179..198 (unused; implicit 0 in the C initializer)
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	},
}
