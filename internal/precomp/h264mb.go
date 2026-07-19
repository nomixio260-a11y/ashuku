package precomp

// H.264 マクロブロック層のトランスコーダ。
//
// **1本の走査コード**が capture(元ビット→算術)と rebuild(算術→ビット)の
// 両方向を駆動する。各構文要素サイトは t.ue()/t.se()/… を呼ぶだけで、
// capture では元 CAVLC ビットを読んで文脈算術に書き、rebuild では算術から
// 読み戻して CAVLC ビットを書く。方向ごとにコードを複製しないので、
// 両方向の非対称バグが構造的に起きない。ピクセル復号は一切しない
// (nC 導出に必要な 4x4 ブロックごとの非ゼロ係数数だけを追跡する)。

// h264T はトランスコーダ状態。
type h264T struct {
	capture bool
	br      *h264Reader // capture: 元スライスビット
	bw      *h264Writer // rebuild: 出力ビット
	enc     *rangeEncoder
	dec     *rangeDecoder
	m       *h264Model
}

// --- 要素プリミティブ(capture: bits→arith / rebuild: arith→bits) ---

func (t *h264T) ue(c *numCtx) (uint32, error) {
	if t.capture {
		v, err := t.br.ue()
		if err != nil {
			return 0, err
		}
		c.encode(t.enc, v)
		return v, nil
	}
	v := c.decode(t.dec)
	t.bw.ue(v)
	return v, nil
}

func (t *h264T) se(c *numCtx) (int32, error) {
	if t.capture {
		v, err := t.br.se()
		if err != nil {
			return 0, err
		}
		c.encode(t.enc, zigzagS(v))
		return v, nil
	}
	v := unzigzagS(c.decode(t.dec))
	t.bw.se(v)
	return v, nil
}

func (t *h264T) te(c *numCtx, rangeMax int) (uint32, error) {
	if t.capture {
		v, err := t.br.te(rangeMax)
		if err != nil {
			return 0, err
		}
		c.encode(t.enc, v)
		return v, nil
	}
	v := c.decode(t.dec)
	t.bw.te(v, rangeMax)
	return v, nil
}

func (t *h264T) bit(mdl *bitModel) (uint32, error) {
	if t.capture {
		v, err := t.br.u1()
		if err != nil {
			return 0, err
		}
		t.enc.encodeBit(mdl, int(v))
		return v, nil
	}
	v := uint32(t.dec.decodeBit(mdl))
	t.bw.u1(v)
	return v, nil
}

func (t *h264T) bits(models []bitModel, n int) (uint32, error) {
	var v uint32
	for i := 0; i < n; i++ {
		b, err := t.bit(&models[i])
		if err != nil {
			return 0, err
		}
		v = v<<1 | b
	}
	return v, nil
}

// flag は算術ストリームにだけ存在する制御ビット(ビット列には出ない)。
// capture 側は実測値 v を符号化し、rebuild 側は復号値を返す。
func (t *h264T) flag(mdl *bitModel, v bool) bool {
	if t.capture {
		b := 0
		if v {
			b = 1
		}
		t.enc.encodeBit(mdl, b)
		return v
	}
	return t.dec.decodeBit(mdl) == 1
}

// --- CBP の me(v) マッピング(Table 9-4、4:2:0) ---

var cbpIntraTable = [48]uint8{
	47, 31, 15, 0, 23, 27, 29, 30, 7, 11, 13, 14, 39, 43, 45, 46,
	16, 3, 5, 10, 12, 19, 21, 26, 28, 35, 37, 42, 44, 1, 2, 4,
	8, 17, 18, 20, 24, 6, 9, 22, 25, 32, 33, 34, 36, 40, 38, 41,
}
var cbpInterTable = [48]uint8{
	0, 16, 1, 2, 4, 8, 32, 3, 5, 10, 12, 15, 47, 7, 11, 13,
	14, 6, 9, 31, 35, 37, 42, 44, 33, 34, 36, 40, 39, 43, 45, 46,
	17, 18, 20, 24, 19, 21, 26, 28, 23, 27, 29, 30, 22, 25, 38, 41,
}

var cbpIntraInv, cbpInterInv [48]uint8

func init() {
	for i, v := range cbpIntraTable {
		cbpIntraInv[v] = uint8(i)
	}
	for i, v := range cbpInterTable {
		cbpInterInv[v] = uint8(i)
	}
}

// cbp は me(v) を要素化する(算術側は cbp 値そのものを6ビットで持つ)。
func (t *h264T) cbp(models []bitModel, intra bool) (int, error) {
	if t.capture {
		code, err := t.br.ue()
		if err != nil {
			return 0, err
		}
		if code >= 48 {
			return 0, errH264Unsupported
		}
		var v uint8
		if intra {
			v = cbpIntraTable[code]
		} else {
			v = cbpInterTable[code]
		}
		for i := 5; i >= 0; i-- {
			t.enc.encodeBit(&models[5-i], int(v>>uint(i))&1)
		}
		return int(v), nil
	}
	var v uint8
	for i := 5; i >= 0; i-- {
		v = v<<1 | uint8(t.dec.decodeBit(&models[5-i]))
	}
	if intra {
		t.bw.ue(uint32(cbpIntraInv[v]))
	} else {
		t.bw.ue(uint32(cbpInterInv[v]))
	}
	return int(v), nil
}

// --- 4x4 ブロックの非ゼロ係数数マップ(nC 導出用) ---

type h264NZ struct {
	mbW, mbH int
	luma     []int16 // [mbH*4][mbW*4]
	chroma   [2][]int16
	sliceID  []int32 // MB ごと
	curSlice int32
}

func newH264NZ(mbW, mbH int) *h264NZ {
	nz := &h264NZ{mbW: mbW, mbH: mbH}
	nz.luma = make([]int16, mbW*4*mbH*4)
	nz.chroma[0] = make([]int16, mbW*2*mbH*2)
	nz.chroma[1] = make([]int16, mbW*2*mbH*2)
	nz.sliceID = make([]int32, mbW*mbH)
	for i := range nz.sliceID {
		nz.sliceID[i] = -1
	}
	return nz
}

func (nz *h264NZ) setLuma(mbAddr, blk, count int) {
	x := (mbAddr%nz.mbW)*4 + lumaBlkX[blk]
	y := (mbAddr/nz.mbW)*4 + lumaBlkY[blk]
	nz.luma[y*nz.mbW*4+x] = int16(count)
}

func (nz *h264NZ) setChroma(mbAddr, comp, blk, count int) {
	x := (mbAddr%nz.mbW)*2 + blk&1
	y := (mbAddr/nz.mbW)*2 + blk>>1
	nz.chroma[comp][y*nz.mbW*2+x] = int16(count)
}

// lumaNC は luma 4x4 ブロック blk の nC(9.2.1)。
func (nz *h264NZ) lumaNC(mbAddr, blk int) int {
	x := (mbAddr%nz.mbW)*4 + lumaBlkX[blk]
	y := (mbAddr/nz.mbW)*4 + lumaBlkY[blk]
	nA, aOK := nz.lumaAt(mbAddr, x-1, y)
	nB, bOK := nz.lumaAt(mbAddr, x, y-1)
	return combineNC(nA, aOK, nB, bOK)
}

func (nz *h264NZ) lumaAt(curMB, x, y int) (int, bool) {
	if x < 0 || y < 0 {
		return 0, false
	}
	mb := (y/4)*nz.mbW + x/4
	if nz.sliceID[mb] != nz.curSlice {
		return 0, false
	}
	return int(nz.luma[y*nz.mbW*4+x]), true
}

func (nz *h264NZ) chromaNC(mbAddr, comp, blk int) int {
	x := (mbAddr%nz.mbW)*2 + blk&1
	y := (mbAddr/nz.mbW)*2 + blk>>1
	nA, aOK := nz.chromaAt(comp, x-1, y)
	nB, bOK := nz.chromaAt(comp, x, y-1)
	return combineNC(nA, aOK, nB, bOK)
}

func (nz *h264NZ) chromaAt(comp, x, y int) (int, bool) {
	if x < 0 || y < 0 {
		return 0, false
	}
	mb := (y/2)*nz.mbW + x/2
	if nz.sliceID[mb] != nz.curSlice {
		return 0, false
	}
	return int(nz.chroma[comp][y*nz.mbW*2+x]), true
}

func combineNC(nA int, aOK bool, nB int, bOK bool) int {
	switch {
	case aOK && bOK:
		return (nA + nB + 1) >> 1
	case aOK:
		return nA
	case bOK:
		return nB
	}
	return 0
}

// luma4x4BlkIdx → MB 内 4x4 座標(Figure 6-10 の走査順)。
var lumaBlkX = [16]int{0, 1, 0, 1, 2, 3, 2, 3, 0, 1, 0, 1, 2, 3, 2, 3}
var lumaBlkY = [16]int{0, 0, 1, 1, 0, 0, 1, 1, 2, 2, 3, 3, 2, 2, 3, 3}

// lumaNbrNZ は luma ブロック blk の (dx,dy) 隣接 4x4 の非ゼロ数。CABAC の
// cbf 文脈用。利用不可(スライス外)の隣接は I スライスでは 64 扱い
// (FFmpeg fill_caches の CABAC&&!INTRA?0:64 で、I は INTRA なので 64)。
func (nz *h264NZ) lumaNbrNZ(mbAddr, blk, dx, dy int) int {
	return nz.lumaNbrNZDef(mbAddr, blk, dx, dy, 64)
}

// lumaNbrNZDef は既定値指定つき(inter 現MBは 0、intra は 64)。
func (nz *h264NZ) lumaNbrNZDef(mbAddr, blk, dx, dy, def int) int {
	x := (mbAddr%nz.mbW)*4 + lumaBlkX[blk] + dx
	y := (mbAddr/nz.mbW)*4 + lumaBlkY[blk] + dy
	if v, ok := nz.lumaAt(mbAddr, x, y); ok {
		return v
	}
	return def
}

func (nz *h264NZ) chromaNbrNZ(mbAddr, comp, blk, dx, dy int) int {
	return nz.chromaNbrNZDef(mbAddr, comp, blk, dx, dy, 64)
}

func (nz *h264NZ) chromaNbrNZDef(mbAddr, comp, blk, dx, dy, def int) int {
	x := (mbAddr%nz.mbW)*2 + blk&1 + dx
	y := (mbAddr/nz.mbW)*2 + blk>>1 + dy
	if v, ok := nz.chromaAt(comp, x, y); ok {
		return v
	}
	return def
}

// --- 残差ブロック(9.2): coeff_token → T1符号 → レベル → total_zeros → run ---

// residualBlock は1ブロックをトランスコードし totalCoeff を返す。
// nC: -1=chroma DC。maxCoeff: 16/15/4。
func (t *h264T) residualBlock(nC, maxCoeff int) (int, error) {
	bkt := nCBucket(nC)
	var tc, t1 int
	if t.capture {
		var err error
		tc, t1, err = readCoeffToken(t.br, nC)
		if err != nil {
			return 0, err
		}
		t.m.tc[bkt].encode(t.enc, uint32(tc))
		if tc > 0 {
			t.enc.encodeBit(&t.m.t1Bits[bkt][0], t1>>1)
			t.enc.encodeBit(&t.m.t1Bits[bkt][1], t1&1)
		}
	} else {
		tc = int(t.m.tc[bkt].decode(t.dec))
		if tc > 0 {
			hi := t.dec.decodeBit(&t.m.t1Bits[bkt][0])
			lo := t.dec.decodeBit(&t.m.t1Bits[bkt][1])
			t1 = hi<<1 | lo
		}
		if tc > maxCoeff || t1 > tc || t1 > 3 {
			return 0, errH264Bits
		}
		writeCoeffToken(t.bw, nC, tc, t1)
	}
	if tc == 0 {
		return 0, nil
	}
	// trailing one の符号
	for i := 0; i < t1; i++ {
		if _, err := t.bit(&t.m.t1Sign[i]); err != nil {
			return 0, err
		}
	}
	// レベル(suffixLength は両方向で同一に進化する)
	sl := 0
	if tc > 10 && t1 < 3 {
		sl = 1
	}
	for i := t1; i < tc; i++ {
		var lc int
		if t.capture {
			var err error
			lc, err = readLevelCode(t.br, sl)
			if err != nil {
				return 0, err
			}
			t.m.level[sl].encode(t.enc, uint32(lc))
		} else {
			lc = int(t.m.level[sl].decode(t.dec))
			writeLevelCode(t.bw, lc, sl)
		}
		// 実効レベルを求めて suffixLength を更新(9.2.2.1)
		eff := lc
		if i == t1 && t1 < 3 {
			eff += 2
		}
		var level int
		if eff%2 == 0 {
			level = (eff + 2) / 2
		} else {
			level = -(eff + 1) / 2
		}
		if sl == 0 {
			sl = 1
		}
		abs := level
		if abs < 0 {
			abs = -abs
		}
		if abs > 3<<uint(sl-1) && sl < 6 {
			sl++
		}
	}
	// total_zeros
	zerosLeft := 0
	if tc < maxCoeff {
		chromaDC := nC == -1
		if t.capture {
			tz, err := readTotalZeros(t.br, tc, chromaDC)
			if err != nil {
				return 0, err
			}
			if chromaDC {
				t.m.tzChroma.encode(t.enc, uint32(tz))
			} else {
				t.m.tzWide[tcBucket(tc)].encode(t.enc, uint32(tz))
			}
			zerosLeft = tz
		} else {
			var tz int
			if chromaDC {
				tz = int(t.m.tzChroma.decode(t.dec))
			} else {
				tz = int(t.m.tzWide[tcBucket(tc)].decode(t.dec))
			}
			if tz > maxCoeff-tc {
				return 0, errH264Bits
			}
			writeTotalZeros(t.bw, tc, tz, chromaDC)
			zerosLeft = tz
		}
	}
	// run_before
	for i := 0; i < tc-1 && zerosLeft > 0; i++ {
		var run int
		if t.capture {
			var err error
			run, err = readRunBefore(t.br, zerosLeft)
			if err != nil {
				return 0, err
			}
			t.m.runB[zlBucket(zerosLeft)].encode(t.enc, uint32(run))
		} else {
			run = int(t.m.runB[zlBucket(zerosLeft)].decode(t.dec))
			if run > zerosLeft {
				return 0, errH264Bits
			}
			writeRunBefore(t.bw, zerosLeft, run)
		}
		zerosLeft -= run
	}
	return tc, nil
}

// --- マクロブロック層(7.3.5) ---

// transcodeMB は1MB をトランスコードする。
func (t *h264T) transcodeMB(sl *h264Slice, nz *h264NZ, mbAddr int) error {
	isP := sl.sliceType == 0 || sl.sliceType == 5
	var mbType uint32
	var err error
	if isP {
		mbType, err = t.ue(t.m.mbTypeP)
	} else {
		mbType, err = t.ue(t.m.mbTypeI)
	}
	if err != nil {
		return err
	}

	intraType := -1 // I テーブルでの mb_type(0=I4x4, 1..24=I16x16, 25=PCM)
	if isP {
		if mbType >= 5 {
			intraType = int(mbType) - 5
		}
	} else {
		intraType = int(mbType)
	}
	if intraType == 25 {
		return errH264Unsupported // I_PCM は対象外
	}
	if intraType > 25 || (isP && intraType < 0 && mbType > 4) {
		return errH264Bits
	}

	cbp := 0
	i16 := false
	switch {
	case intraType == 0: // I_4x4
		for b := 0; b < 16; b++ {
			pf, err := t.bit(&t.m.prevIntra[0])
			if err != nil {
				return err
			}
			if pf == 0 {
				if _, err := t.bits(t.m.remIntra, 3); err != nil {
					return err
				}
			}
		}
		if _, err := t.ue(t.m.chromaPr); err != nil {
			return err
		}
		c, err := t.cbp(t.m.cbpBitsI, true)
		if err != nil {
			return err
		}
		cbp = c
	case intraType >= 1: // I_16x16
		i16 = true
		tt := intraType - 1
		cbp = ((tt / 12) * 15) | (((tt / 4) % 3) << 4)
		if _, err := t.ue(t.m.chromaPr); err != nil {
			return err
		}
	default: // インター(P)
		if err := t.transcodePMotion(sl, int(mbType)); err != nil {
			return err
		}
		c, err := t.cbp(t.m.cbpBitsP, false)
		if err != nil {
			return err
		}
		cbp = c
	}

	if cbp > 0 || i16 {
		if _, err := t.se(t.m.qpDelta); err != nil {
			return err
		}
	}

	// 残差
	if i16 {
		// DC(16係数): nC はブロック0の近傍から
		if _, err := t.residualBlock(nz.lumaNC(mbAddr, 0), 16); err != nil {
			return err
		}
	}
	cbpLuma := cbp & 15
	maxL := 16
	if i16 {
		maxL = 15
	}
	for blk8 := 0; blk8 < 4; blk8++ {
		for j := 0; j < 4; j++ {
			blk := blk8*4 + j
			if cbpLuma&(1<<uint(blk8)) != 0 {
				tc, err := t.residualBlock(nz.lumaNC(mbAddr, blk), maxL)
				if err != nil {
					return err
				}
				nz.setLuma(mbAddr, blk, tc)
			} else {
				nz.setLuma(mbAddr, blk, 0)
			}
		}
	}
	cbpChroma := cbp >> 4
	if cbpChroma > 0 {
		for c := 0; c < 2; c++ { // DC(4係数、nC=-1)
			if _, err := t.residualBlock(-1, 4); err != nil {
				return err
			}
		}
	}
	for c := 0; c < 2; c++ {
		for blk := 0; blk < 4; blk++ {
			if cbpChroma == 2 {
				tc, err := t.residualBlock(nz.chromaNC(mbAddr, c, blk), 15)
				if err != nil {
					return err
				}
				nz.setChroma(mbAddr, c, blk, tc)
			} else {
				nz.setChroma(mbAddr, c, blk, 0)
			}
		}
	}
	return nil
}

// transcodePMotion は P マクロブロックの予測部(7.3.5.1 / 7.3.5.2)。
func (t *h264T) transcodePMotion(sl *h264Slice, mbType int) error {
	nRef := sl.numRefIdxL0
	switch mbType {
	case 0: // 16x16
		if nRef > 1 {
			if _, err := t.te(t.m.refIdx, nRef-1); err != nil {
				return err
			}
		}
		return t.mvdPair()
	case 1, 2: // 16x8 / 8x16
		for p := 0; p < 2; p++ {
			if nRef > 1 {
				if _, err := t.te(t.m.refIdx, nRef-1); err != nil {
					return err
				}
			}
		}
		for p := 0; p < 2; p++ {
			if err := t.mvdPair(); err != nil {
				return err
			}
		}
		return nil
	case 3, 4: // P_8x8 / P_8x8ref0
		var subType [4]uint32
		for i := 0; i < 4; i++ {
			v, err := t.ue(t.m.subMbType)
			if err != nil {
				return err
			}
			if v > 3 {
				return errH264Bits
			}
			subType[i] = v
		}
		if mbType == 3 && nRef > 1 {
			for i := 0; i < 4; i++ {
				if _, err := t.te(t.m.refIdx, nRef-1); err != nil {
					return err
				}
			}
		}
		nParts := [4]int{1, 2, 2, 4}
		for i := 0; i < 4; i++ {
			for p := 0; p < nParts[subType[i]]; p++ {
				if err := t.mvdPair(); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return errH264Bits
}

func (t *h264T) mvdPair() error {
	if _, err := t.se(t.m.mvd[0]); err != nil {
		return err
	}
	_, err := t.se(t.m.mvd[1])
	return err
}

// --- スライスデータ全体(7.3.4) ---

// transcodeSliceData はスライスの MB 群をトランスコードする。
// capture: br はヘッダ直後に位置。rebuild: bw にヘッダまで書き込み済み。
func (t *h264T) transcodeSliceData(sps *h264SPS, sl *h264Slice, nz *h264NZ) error {
	isP := sl.sliceType == 0 || sl.sliceType == 5
	total := sps.picWidthInMbs * sps.picHeightInMbs
	curr := sl.firstMB
	nz.curSlice++
	more := true
	for more {
		if curr >= total {
			return errH264Bits
		}
		if isP {
			run, err := t.ue(t.m.skipRun)
			if err != nil {
				return err
			}
			if int(run) > total-curr {
				return errH264Bits
			}
			for i := uint32(0); i < run; i++ {
				t.markSkip(nz, curr)
				curr++
			}
			if run > 0 {
				more = t.flag(&t.m.contBit[0], t.capture && t.br.moreRBSPData())
				if !more {
					break
				}
			}
			if curr >= total {
				return errH264Bits
			}
		}
		nz.sliceID[curr] = nz.curSlice
		if err := t.transcodeMB(sl, nz, curr); err != nil {
			return err
		}
		curr++
		more = t.flag(&t.m.contBit[1], t.capture && t.br.moreRBSPData())
	}
	if t.capture && t.br.moreRBSPData() {
		return errH264Bits // 走査終了後に未消費データ(非対応構文の疑い)
	}
	return nil
}

func (t *h264T) markSkip(nz *h264NZ, mbAddr int) {
	nz.sliceID[mbAddr] = nz.curSlice
	for blk := 0; blk < 16; blk++ {
		nz.setLuma(mbAddr, blk, 0)
	}
	for c := 0; c < 2; c++ {
		for blk := 0; blk < 4; blk++ {
			nz.setChroma(mbAddr, c, blk, 0)
		}
	}
}
