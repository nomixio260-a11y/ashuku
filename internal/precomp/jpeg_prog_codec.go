package precomp

// プログレッシブ JPEG のスキャン復号・再符号化(ITU T.81 G 節、
// libjpeg の標準アルゴリズムと同一の決定的挙動)。

import (
	"bytes"
	"errors"
	"fmt"
)

// decodeProgressive は全スキャンを復号して係数配列(baseline と同じ
// MCU順レイアウト、DC は走査順差分ではなく**絶対値**)を返す。
func (pf *progFrame) decodeProgressive(orig []byte) ([][]int16, error) {
	f := pf.frame
	nMCU := f.mcuX * f.mcuY
	coeff := make([][]int16, len(f.comps))
	for ci := range f.comps {
		coeff[ci] = make([]int16, nMCU*f.comps[ci].blocksMC*64)
	}
	for si := range pf.scans {
		sc := &pf.scans[si]
		if err := pf.decodeScanProg(orig, sc, coeff); err != nil {
			return nil, fmt.Errorf("scan%d Ss=%d Se=%d Ah=%d Al=%d ncomp=%d: %w", si, sc.ss, sc.se, sc.ah, sc.al, len(sc.comps), err)
		}
	}
	return coeff, nil
}

// blockIndex は成分 ci のラスタ位置 (bx,by) → MCU順ブロック番号。
func (pf *progFrame) blockIndex(ci, bx, by int) int {
	f := pf.frame
	c := &f.comps[ci]
	mcux, bh := bx/c.h, bx%c.h
	mcuy, bv := by/c.v, by%c.v
	mcu := mcuy*f.mcuX + mcux
	return mcu*c.blocksMC + bv*c.h + bh
}

func (pf *progFrame) decodeScanProg(orig []byte, sc *progScan, coeff [][]int16) error {
	r := &bitReader{data: orig[sc.entStart:sc.entEnd]}
	interleaved := len(sc.comps) > 1
	f := pf.frame

	if sc.ss == 0 { // DC スキャン
		if sc.se != 0 {
			return errors.New("DC スキャンの Se が不正")
		}
		var pred [4]int
		var nUnit, unit int
		if interleaved {
			nUnit = f.mcuX * f.mcuY
		} else {
			ci := sc.comps[0].ci
			nUnit = pf.blocksW[ci] * pf.blocksH[ci]
		}
		for unit = 0; unit < nUnit; unit++ {
			if sc.restart > 0 && unit > 0 && unit%sc.restart == 0 {
				if !r.takeRestart() {
					return errors.New("リスタートマーカがない")
				}
				for i := range pred {
					pred[i] = 0
				}
			}
			if interleaved {
				mcu := unit
				for s := range sc.comps {
					ci := sc.comps[s].ci
					c := &f.comps[ci]
					for b := 0; b < c.blocksMC; b++ {
						base := (mcu*c.blocksMC + b) * 64
						if err := decodeDCUnit(r, sc, s, &pred, coeff[ci][base:base+64]); err != nil {
							return err
						}
					}
				}
			} else {
				ci := sc.comps[0].ci
				bw := pf.blocksW[ci]
				bx, by := unit%bw, unit/bw
				base := pf.blockIndex(ci, bx, by) * 64
				if err := decodeDCUnit(r, sc, 0, &pred, coeff[ci][base:base+64]); err != nil {
					return err
				}
			}
		}
		return nil
	}

	// AC スキャン(プログレッシブでは必ず単一成分)
	if interleaved {
		return errors.New("AC スキャンがインターリーブ")
	}
	ci := sc.comps[0].ci
	acT := sc.ac[sc.comps[0].ta]
	if acT == nil {
		return errors.New("AC テーブルがない")
	}
	bw, bh := pf.blocksW[ci], pf.blocksH[ci]
	eobrun := 0
	for by := 0; by < bh; by++ {
		for bx := 0; bx < bw; bx++ {
			unit := by*bw + bx
			if sc.restart > 0 && unit > 0 && unit%sc.restart == 0 {
				if !r.takeRestart() {
					return errors.New("リスタートマーカがない")
				}
				eobrun = 0
			}
			base := pf.blockIndex(ci, bx, by) * 64
			blk := coeff[ci][base : base+64]
			var err error
			if sc.ah == 0 {
				eobrun, err = decodeACFirst(r, acT, sc, blk, eobrun)
			} else {
				eobrun, err = decodeACRefine(r, acT, sc, blk, eobrun)
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func decodeDCUnit(r *bitReader, sc *progScan, s int, pred *[4]int, blk []int16) error {
	if sc.ah == 0 { // 最初の DC スキャン
		dcT := sc.dc[sc.comps[s].td]
		if dcT == nil {
			return errors.New("DC テーブルがない")
		}
		t, err := r.decodeHuff(dcT)
		if err != nil {
			return err
		}
		if t > 15 {
			return errors.New("DC カテゴリ不正")
		}
		diff := extend(r.readBits(int(t)), int(t))
		pred[s] += diff
		blk[0] = int16(pred[s] << uint(sc.al))
	} else { // DC 逐次近似の追加ビット
		if r.readBit() != 0 {
			blk[0] |= 1 << uint(sc.al)
		}
	}
	return nil
}

func decodeACFirst(r *bitReader, acT *huffTable, sc *progScan, blk []int16, eobrun int) (int, error) {
	if eobrun > 0 {
		return eobrun - 1, nil
	}
	k := sc.ss
	for k <= sc.se {
		rs, err := r.decodeHuff(acT)
		if err != nil {
			return 0, err
		}
		rr, ssz := int(rs>>4), int(rs&15)
		if ssz == 0 {
			if rr == 15 {
				k += 16
				continue
			}
			eobrun = (1 << uint(rr)) - 1
			if rr > 0 {
				eobrun += r.readBits(rr)
			}
			return eobrun, nil
		}
		k += rr
		if k > sc.se {
			return 0, errors.New("AC 位置範囲外")
		}
		blk[k] = int16(extend(r.readBits(ssz), ssz) << uint(sc.al))
		k++
	}
	return 0, nil
}

func decodeACRefine(r *bitReader, acT *huffTable, sc *progScan, blk []int16, eobrun int) (int, error) {
	p1 := int16(1) << uint(sc.al)  // 新規有意の正値
	m1 := int16(-1) << uint(sc.al) // 新規有意の負値
	refine := func(k int) {
		if blk[k] == 0 {
			return
		}
		if r.readBit() != 0 && blk[k]&p1 == 0 {
			if blk[k] >= 0 {
				blk[k] += p1
			} else {
				blk[k] += m1
			}
		}
	}
	k := sc.ss
	if eobrun == 0 {
		for k <= sc.se {
			rs, err := r.decodeHuff(acT)
			if err != nil {
				return 0, err
			}
			rr, ssz := int(rs>>4), int(rs&15)
			var s int16 // 新規有意係数の値(0=なし)
			if ssz != 0 {
				if ssz != 1 {
					return 0, errors.New("AC 補正の振幅が不正")
				}
				if r.readBit() != 0 {
					s = p1
				} else {
					s = m1
				}
			} else {
				if rr != 15 {
					eobrun = 1 << uint(rr)
					if rr > 0 {
						eobrun += r.readBits(rr)
					}
					break
				}
				// rr==15: ゼロ履歴16個(補正ビットを挟みながら進む)
			}
			// rr 個のゼロ履歴を消化しつつ、非ゼロ履歴を補正する
			for k <= sc.se {
				if blk[k] != 0 {
					refine(k)
				} else {
					if rr == 0 {
						break
					}
					rr--
				}
				k++
			}
			// 外側 for の k++ に相当(ZRL でも新規係数でも常に1つ進む)
			if s != 0 && k <= sc.se {
				blk[k] = s
			}
			k++
		}
	}
	if eobrun > 0 {
		for ; k <= sc.se; k++ {
			refine(k)
		}
		eobrun--
	}
	return eobrun, nil
}

// ---- 再符号化(libjpeg と同一の決定的アルゴリズム) ----

// progWriter はスキャン1本のビットライタ+EOBRUN/補正ビットバッファ。
type progWriter struct {
	w       bitWriter
	acT     *huffTable
	eobrun  int
	bitsBuf []int // EOBRUN/シンボル確定待ちの補正ビット(BE バッファ)
}

func (pw *progWriter) emitHuff(t *huffTable, sym int) {
	pw.w.writeBits(uint32(t.encCode[sym]), int(t.encSize[sym]))
}

func (pw *progWriter) emitBufferedBits() {
	for _, b := range pw.bitsBuf {
		pw.w.writeBits(uint32(b), 1)
	}
	pw.bitsBuf = pw.bitsBuf[:0]
}

// flushEOB は保留中の EOBRUN(と補正ビット)を書き出す。
func (pw *progWriter) flushEOB() {
	if pw.eobrun > 0 {
		nbits := 0
		for v := pw.eobrun; v > 1; v >>= 1 {
			nbits++
		}
		pw.emitHuff(pw.acT, nbits<<4)
		if nbits > 0 {
			pw.w.writeBits(uint32(pw.eobrun-(1<<uint(nbits))), nbits)
		}
		pw.eobrun = 0
	}
	pw.emitBufferedBits()
}

// encodeProgressive は係数から全スキャンのエントロピーを再符号化し、
// スキャンごとのバイト列を返す。
func (pf *progFrame) encodeProgressive(coeff [][]int16) ([][]byte, error) {
	f := pf.frame
	out := make([][]byte, len(pf.scans))
	for si := range pf.scans {
		sc := &pf.scans[si]
		pw := &progWriter{}
		interleaved := len(sc.comps) > 1
		rst := 0
		emitRestart := func() {
			pw.flushEOB()
			pw.w.pad()
			pw.w.out = append(pw.w.out, 0xFF, byte(mRST0+rst))
			rst = (rst + 1) & 7
		}

		if sc.ss == 0 { // DC スキャン
			var pred [4]int
			var nUnit int
			if interleaved {
				nUnit = f.mcuX * f.mcuY
			} else {
				ci := sc.comps[0].ci
				nUnit = pf.blocksW[ci] * pf.blocksH[ci]
			}
			for unit := 0; unit < nUnit; unit++ {
				if sc.restart > 0 && unit > 0 && unit%sc.restart == 0 {
					emitRestart()
					for i := range pred {
						pred[i] = 0
					}
				}
				if interleaved {
					for s := range sc.comps {
						ci := sc.comps[s].ci
						c := &f.comps[ci]
						for b := 0; b < c.blocksMC; b++ {
							base := (unit*c.blocksMC + b) * 64
							pf.encodeDCUnit(pw, sc, s, &pred, coeff[ci][base:base+64])
						}
					}
				} else {
					ci := sc.comps[0].ci
					bw := pf.blocksW[ci]
					base := pf.blockIndex(ci, unit%bw, unit/bw) * 64
					pf.encodeDCUnit(pw, sc, 0, &pred, coeff[ci][base:base+64])
				}
			}
		} else { // AC スキャン(単一成分)
			if interleaved {
				return nil, errors.New("AC スキャンがインターリーブ")
			}
			ci := sc.comps[0].ci
			pw.acT = sc.ac[sc.comps[0].ta]
			if pw.acT == nil {
				return nil, errors.New("AC テーブルがない")
			}
			bw, bh := pf.blocksW[ci], pf.blocksH[ci]
			for by := 0; by < bh; by++ {
				for bx := 0; bx < bw; bx++ {
					unit := by*bw + bx
					if sc.restart > 0 && unit > 0 && unit%sc.restart == 0 {
						emitRestart()
					}
					base := pf.blockIndex(ci, bx, by) * 64
					blk := coeff[ci][base : base+64]
					if sc.ah == 0 {
						pf.encodeACFirst(pw, sc, blk)
					} else {
						pf.encodeACRefine(pw, sc, blk)
					}
				}
			}
		}
		pw.flushEOB()
		pw.w.pad()
		out[si] = pw.w.out
	}
	return out, nil
}

func (pf *progFrame) encodeDCUnit(pw *progWriter, sc *progScan, s int, pred *[4]int, blk []int16) {
	if sc.ah == 0 {
		dcT := sc.dc[sc.comps[s].td]
		v := int(blk[0]) >> uint(sc.al)
		diff := v - pred[s]
		pred[s] = v
		t := magCat(diff)
		pw.emitHuff(dcT, t)
		pw.w.writeBits(mantissa(diff, t), t)
	} else {
		pw.w.writeBits(uint32(int(blk[0])>>uint(sc.al))&1, 1)
	}
}

func (pf *progFrame) encodeACFirst(pw *progWriter, sc *progScan, blk []int16) {
	al := uint(sc.al)
	run := 0
	hasCoef := false
	for k := sc.ss; k <= sc.se; k++ {
		v := int(blk[k])
		if v < 0 {
			v = -((-v) >> al)
		} else {
			v >>= al
		}
		if v == 0 {
			run++
			continue
		}
		if !hasCoef {
			pw.flushEOB() // ブロックに係数あり → 保留中の EOBRUN を先に出す
			hasCoef = true
		}
		for run > 15 {
			pw.emitHuff(pw.acT, 0xF0)
			run -= 16
		}
		ssz := magCat(v)
		pw.emitHuff(pw.acT, run<<4|ssz)
		pw.w.writeBits(mantissa(v, ssz), ssz)
		run = 0
	}
	if run > 0 { // 末尾がゼロ → この EOB は EOBRUN に合流
		pw.eobrun++
		if pw.eobrun == 0x7FFF {
			pw.flushEOB()
		}
	}
}

func (pf *progFrame) encodeACRefine(pw *progWriter, sc *progScan, blk []int16) {
	al := uint(sc.al)
	// absvals: このスキャン精度での絶対値。EOB = 新規有意(=1)の最終位置+1
	var absv [64]int
	eob := sc.ss
	for k := sc.ss; k <= sc.se; k++ {
		v := int(blk[k])
		if v < 0 {
			v = -v
		}
		v >>= al
		absv[k] = v
		if v == 1 {
			eob = k + 1
		}
	}
	run := 0
	var pending []int // このブロック内・シンボル確定待ちの補正ビット
	for k := sc.ss; k <= sc.se; k++ {
		v := absv[k]
		if v == 0 {
			run++
			continue
		}
		// ZRL 折り込み判定は(新規有意だけでなく)既存有意の直前でも行う
		// (libjpeg と同一)。既存有意で発行した場合、その係数の補正ビットは
		// ZRL には載らず次のシンボルへ持ち越される。
		for run > 15 && k < eob {
			pw.flushEOB()
			pw.emitHuff(pw.acT, 0xF0)
			run -= 16
			for _, b := range pending {
				pw.w.writeBits(uint32(b), 1)
			}
			pending = pending[:0]
		}
		if v > 1 { // 既に有意 → 補正ビット(直近シンボルの後に出す)
			pending = append(pending, v&1)
			continue
		}
		// v==1: 新規有意
		pw.flushEOB()
		pw.emitHuff(pw.acT, run<<4|1)
		sb := 0
		if blk[k] >= 0 {
			sb = 1
		}
		pw.w.writeBits(uint32(sb), 1)
		run = 0
		for _, b := range pending {
			pw.w.writeBits(uint32(b), 1)
		}
		pending = pending[:0]
	}
	if run > 0 || len(pending) > 0 {
		pw.eobrun++
		pw.bitsBuf = append(pw.bitsBuf, pending...)
		// libjpeg: EOBRUN カウンタ上限のほか、補正ビットバッファの溢れ
		// (MAX_CORR_BITS=1000、次ブロックで最大 DCTSIZE2-1 ビット増える)
		// を避けるため BE > 1000-64+1 でも強制フラッシュする。この一致が
		// ないと補正ビット密度の高い画像でビット一致検証に失敗する。
		if pw.eobrun == 0x7FFF || len(pw.bitsBuf) > 1000-64+1 {
			pw.flushEOB()
		}
	}
}

// TryUnwrapJPEGProgressive はプログレッシブ JPEG を分解する。
// 再符号化が全スキャンでビット一致する場合のみ採用。
func TryUnwrapJPEGProgressive(orig []byte, maxPlain int64) (*JPEGUnwrapped, bool) {
	pf, err := parseProgressive(orig)
	if err != nil {
		return nil, false
	}
	f := pf.frame
	nMCU := int64(f.mcuX) * int64(f.mcuY)
	var totalBlocks int64
	for i := range f.comps {
		totalBlocks += nMCU * int64(f.comps[i].blocksMC)
	}
	if totalBlocks*64*2 > maxPlain {
		return nil, false
	}
	coeff, err := pf.decodeProgressive(orig)
	if err != nil {
		return nil, false
	}
	// ビット一致検証: 全スキャンを再符号化して元エントロピーと比較
	scans, err := pf.encodeProgressive(coeff)
	if err != nil {
		return nil, false
	}
	for si := range pf.scans {
		sc := &pf.scans[si]
		if !bytes.Equal(scans[si], orig[sc.entStart:sc.entEnd]) {
			return nil, false
		}
	}
	// 係数を文脈算術符号化(DC は絶対値なので走査順差分に変換して
	// baseline コーダをそのまま使う)
	for ci := range coeff {
		absToDiffDC(coeff[ci], f.dcResetStride2(ci))
	}
	arith := f.encodeCoefficients(coeff)
	rt := f.decodeCoefficients(arith)
	for ci := range coeff {
		diffToAbsDC(rt[ci], f.dcResetStride2(ci))
		diffToAbsDC(coeff[ci], f.dcResetStride2(ci))
	}
	if !coeffEqual(coeff, rt) {
		return nil, false
	}
	// planes 候補も見積もる(スクショ系はこちらが勝つ)
	planes := serializePlanesAbs(f, coeff)
	probe := jpegProbeEncoder.EncodeAll(planes, make([]byte, 0, len(planes)/2))
	arithTotal := len(pf.skeleton) + len(arith)
	planesTotal := len(pf.skeleton) + len(probe)
	var payload []byte
	var coder int
	if arithTotal <= planesTotal {
		payload, coder = arith, jpegCoderArith
	} else {
		payload, coder = planes, jpegCoderPlanes
	}
	chosen := arithTotal
	if coder == jpegCoderPlanes {
		chosen = planesTotal
	}
	if chosen >= len(orig) {
		return nil, false
	}
	chunked := make([]byte, 0, len(pf.skeleton)+len(payload))
	chunked = append(chunked, pf.skeleton...)
	chunked = append(chunked, payload...)
	return &JPEGUnwrapped{
		Chunked: chunked,
		Recipe: &JPEGRecipe{
			PrefixLen:   len(pf.skeleton),
			Coder:       coder,
			Progressive: true,
		},
	}, true
}

// ReconstructJPEGProgressive はレシピとチャンク化内容からプログレッシブ
// JPEG をビット単位で戻す。
func ReconstructJPEGProgressive(recipe *JPEGRecipe, chunked []byte) ([]byte, error) {
	if recipe == nil || recipe.PrefixLen < 0 || recipe.PrefixLen > len(chunked) {
		return nil, errors.New("JPEG レシピが不正です")
	}
	skeleton := chunked[:recipe.PrefixLen]
	payload := chunked[recipe.PrefixLen:]
	pf, err := parseProgressive(skeleton)
	if err != nil {
		return nil, err
	}
	f := pf.frame
	var coeff [][]int16
	switch recipe.Coder {
	case jpegCoderArith:
		coeff = f.decodeCoefficients(payload)
		for ci := range coeff {
			diffToAbsDC(coeff[ci], f.dcResetStride2(ci))
		}
	default:
		coeff, err = deserializePlanes(f, payload)
		if err != nil {
			return nil, err
		}
	}
	scans, err := pf.encodeProgressive(coeff)
	if err != nil {
		return nil, err
	}
	// skeleton に各スキャンのエントロピーを挿入
	out := make([]byte, 0, len(skeleton)+len(payload)*2)
	prev := 0
	for si := range pf.scans {
		cut := pf.scans[si].skelEnd
		out = append(out, skeleton[prev:cut]...)
		out = append(out, scans[si]...)
		prev = cut
	}
	out = append(out, skeleton[prev:]...)
	return out, nil
}

// dcResetStride2 は progressive 用の DC リセット間隔(baseline と同じ規則)。
// 注: 算術コーダは DC を差分で扱うため、絶対値との変換に使う。
func (f *jpegFrame) dcResetStride2(ci int) int {
	// 算術コーダの DC 差分は「MCU順・成分内の連続ブロック」で取る。
	// progressive では リスタートはスキャンごとに異なるため、コーダ層では
	// リセットなし(stride 0)で統一する(可逆性は変換の対称性で保たれる)。
	return 0
}

// absToDiffDC は DC 絶対値を MCU 順の差分に変換する(stride=0 固定)。
func absToDiffDC(coeff []int16, stride int) {
	var prev int16
	for dec := 0; dec < len(coeff)/64; dec++ {
		v := coeff[dec*64]
		coeff[dec*64] = v - prev
		prev = v
	}
}

// diffToAbsDC は absToDiffDC の逆。
func diffToAbsDC(coeff []int16, stride int) {
	var acc int16
	for dec := 0; dec < len(coeff)/64; dec++ {
		acc += coeff[dec*64]
		coeff[dec*64] = acc
	}
}

// serializePlanesAbs は絶対 DC のまま平面化する(planes コーダ用)。
func serializePlanesAbs(f *jpegFrame, coeff [][]int16) []byte {
	return serializePlanes(f, coeff)
}
