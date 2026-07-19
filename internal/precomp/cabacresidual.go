package precomp

// H.264 CABAC の残差(有意マップ+レベル)走査。9.3.3.1.1.x / 9.3.2.3。
// 係数値は復号しない(coeff_count と符号ビンだけを消費し近傍 nz を更新)。

// 残差カテゴリ(4:2:0、フレーム、8x8変換なし)。
// cat 0:luma DC, 1:luma AC(I16), 2:luma AC(I4x4/inter), 3:chroma DC, 4:chroma AC。
var cabacSigBase = [5]int{105, 120, 134, 149, 152}  // significant_coeff_flag_offset[0][0..4]
var cabacLastBase = [5]int{166, 181, 195, 210, 213} // last_coeff_flag_offset[0][0..4]
var cabacAbsBase = [5]int{227, 237, 247, 257, 266}  // coeff_abs_level_m1_offset[0..4]
var cabacCbfBase = [5]int{85, 89, 93, 97, 101}      // base_ctx[0..4]

var coeffAbsLevel1Ctx = [8]int{1, 2, 3, 4, 0, 0, 0, 0}
var coeffAbsLevelGt1Ctx = [8]int{5, 5, 5, 5, 6, 7, 8, 9}
var coeffAbsTrans = [2][8]int{{1, 2, 3, 3, 4, 5, 6, 7}, {4, 4, 4, 4, 5, 6, 7, 7}}

// cbfCtxLumaDC は luma DC の coded_block_flag ctxIdx。
func (st *cabacMBState) cbfCtxDC(mb, cat, idx int) int {
	var nza, nzb int32
	if cat == 3 { // chroma DC: cbp16 bits 6+idx
		nza = st.leftCbp(mb) >> (6 + idx) & 1
		nzb = st.topCbp(mb) >> (6 + idx) & 1
	} else { // luma DC: bit 8<<idx
		nza = st.leftCbp(mb) & (0x100 << idx)
		nzb = st.topCbp(mb) & (0x100 << idx)
	}
	ctx := 0
	if nza > 0 {
		ctx++
	}
	if nzb > 0 {
		ctx += 2
	}
	return cabacCbfBase[cat] + ctx
}

// cbfCtxAC は AC ブロックの coded_block_flag ctxIdx(近傍 4x4 nz)。
// 利用不可近傍の既定値は現在 MB の intra/inter で異なる(64/0)。
func (st *cabacMBState) cbfCtxLumaAC(mb, cat, blk int) int {
	def := st.nzDefault()
	nza := st.nz.lumaNbrNZDef(mb, blk, -1, 0, def)
	nzb := st.nz.lumaNbrNZDef(mb, blk, 0, -1, def)
	ctx := 0
	if nza > 0 {
		ctx++
	}
	if nzb > 0 {
		ctx += 2
	}
	return cabacCbfBase[cat] + ctx
}

func (st *cabacMBState) cbfCtxChromaAC(mb, comp, blk int) int {
	def := st.nzDefault()
	nza := st.nz.chromaNbrNZDef(mb, comp, blk, -1, 0, def)
	nzb := st.nz.chromaNbrNZDef(mb, comp, blk, 0, -1, def)
	ctx := 0
	if nza > 0 {
		ctx++
	}
	if nzb > 0 {
		ctx += 2
	}
	return cabacCbfBase[4] + ctx
}

// cabacResidualBlock は1ブロックの残差を走査し coeff_count を返す。
// cbf を自前で読む場合は readCbf=true(cbfCtx を渡す)。
func cabacResidualBlock(sink cabacSink, cat, maxCoeff, cbfCtx int) int {
	if sink.decision(cbfCtx) == 0 {
		return 0
	}
	// 有意マップ
	sigBase := cabacSigBase[cat]
	lastBase := cabacLastBase[cat]
	coeffCount := 0
	last := 0
	for last = 0; last < maxCoeff-1; last++ {
		if sink.decision(sigBase+last) != 0 {
			coeffCount++
			if sink.decision(lastBase+last) != 0 {
				last = maxCoeff
				break
			}
		}
	}
	if last == maxCoeff-1 {
		coeffCount++
	}
	if coeffCount == 0 {
		return 0 // 起き得ないが安全に
	}
	// レベル(逆順)
	absBase := cabacAbsBase[cat]
	nodeCtx := 0
	for i := 0; i < coeffCount; i++ {
		ctx := coeffAbsLevel1Ctx[nodeCtx] + absBase
		if sink.decision(ctx) == 0 {
			nodeCtx = coeffAbsTrans[0][nodeCtx]
			sink.bypass() // 符号
		} else {
			coeffAbs := 2
			ctx = coeffAbsLevelGt1Ctx[nodeCtx] + absBase
			nodeCtx = coeffAbsTrans[1][nodeCtx]
			for coeffAbs < 15 && sink.decision(ctx) != 0 {
				coeffAbs++
			}
			if coeffAbs >= 15 {
				j := 0
				for sink.bypass() != 0 && j < 16+7 {
					j++
				}
				for j > 0 {
					sink.bypass()
					j--
				}
			}
			sink.bypass() // 符号
		}
	}
	return coeffCount
}

// cabacResidual8x8 は cat5(luma 8x8、64係数)を走査する。4:2:0 では cbf は
// 読まない(cbp ビットが唯一のゲート)。有意マップは 8x8 用の文脈写像を使う。
func cabacResidual8x8(sink cabacSink) int {
	coeffCount := 0
	last := 0
	for last = 0; last < 63; last++ {
		if sink.decision(402+int(cabacSig8x8Frame[last])) != 0 {
			coeffCount++
			if sink.decision(417+int(cabacLast8x8[last])) != 0 {
				last = 64
				break
			}
		}
	}
	if last == 63 {
		coeffCount++
	}
	if coeffCount == 0 {
		return 0
	}
	const absBase = 426
	nodeCtx := 0
	for i := 0; i < coeffCount; i++ {
		ctx := coeffAbsLevel1Ctx[nodeCtx] + absBase
		if sink.decision(ctx) == 0 {
			nodeCtx = coeffAbsTrans[0][nodeCtx]
			sink.bypass()
		} else {
			coeffAbs := 2
			ctx = coeffAbsLevelGt1Ctx[nodeCtx] + absBase
			nodeCtx = coeffAbsTrans[1][nodeCtx]
			for coeffAbs < 15 && sink.decision(ctx) != 0 {
				coeffAbs++
			}
			if coeffAbs >= 15 {
				j := 0
				for sink.bypass() != 0 && j < 16+7 {
					j++
				}
				for j > 0 {
					sink.bypass()
					j--
				}
			}
			sink.bypass()
		}
	}
	return coeffCount
}

// cabacResidualMB はマクロブロックの全残差を走査する(I16/4x4/8x8)。
func cabacResidualMB(sink cabacSink, st *cabacMBState, mb int, i16, is8x8 bool, cbp int) bool {
	if is8x8 {
		for i8 := 0; i8 < 4; i8++ {
			cc := 0
			if cbp&(1<<i8) != 0 {
				cc = cabacResidual8x8(sink)
			}
			for j := 0; j < 4; j++ {
				st.nz.setLuma(mb, i8*4+j, cc)
			}
		}
		return cabacResidualChroma(sink, st, mb, cbp)
	}
	if i16 {
		// luma DC(cat0, 16 係数)
		cc := cabacResidualBlock(sink, 0, 16, st.cbfCtxDC(mb, 0, 0))
		if cc > 0 {
			st.cbp16[mb] |= 0x100
		}
		if cbp&15 != 0 {
			for blk := 0; blk < 16; blk++ {
				cc := cabacResidualBlock(sink, 1, 15, st.cbfCtxLumaAC(mb, 1, blk))
				st.nz.setLuma(mb, blk, cc)
			}
		} else {
			for blk := 0; blk < 16; blk++ {
				st.nz.setLuma(mb, blk, 0)
			}
		}
	} else {
		for i8 := 0; i8 < 4; i8++ {
			if cbp&(1<<i8) != 0 {
				for j := 0; j < 4; j++ {
					blk := i8*4 + j
					cc := cabacResidualBlock(sink, 2, 16, st.cbfCtxLumaAC(mb, 2, blk))
					st.nz.setLuma(mb, blk, cc)
				}
			} else {
				for j := 0; j < 4; j++ {
					st.nz.setLuma(mb, i8*4+j, 0)
				}
			}
		}
	}
	return cabacResidualChroma(sink, st, mb, cbp)
}

// cabacResidualChroma は chroma DC/AC の残差走査(4:2:0)。
func cabacResidualChroma(sink cabacSink, st *cabacMBState, mb, cbp int) bool {
	cbpChroma := cbp >> 4
	if cbpChroma != 0 {
		for c := 0; c < 2; c++ { // chroma DC(cat3, 4 係数)
			cc := cabacResidualBlock(sink, 3, 4, st.cbfCtxDC(mb, 3, c))
			if cc > 0 {
				st.cbp16[mb] |= 0x40 << c
			}
		}
	}
	if cbpChroma == 2 {
		for c := 0; c < 2; c++ {
			for blk := 0; blk < 4; blk++ {
				cc := cabacResidualBlock(sink, 4, 15, st.cbfCtxChromaAC(mb, c, blk))
				st.nz.setChroma(mb, c, blk, cc)
			}
		}
	} else {
		for c := 0; c < 2; c++ {
			for blk := 0; blk < 4; blk++ {
				st.nz.setChroma(mb, c, blk, 0)
			}
		}
	}
	return !sink.failed()
}

// i16CBP は I_16x16 の mb_type(1..24)→ cbp(ff_h264_i_mb_type_info)。
func i16CBP(mbt int) int {
	tab := [25]int{0, 0, 0, 0, 0, 16, 16, 16, 16, 32, 32, 32, 32, 15, 15, 15, 15, 31, 31, 31, 31, 47, 47, 47, 47}
	if mbt >= 1 && mbt <= 24 {
		return tab[mbt]
	}
	return 0
}
