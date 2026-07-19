package precomp

// H.264 CAVLC の VLC テーブル(ITU-T H.264 9.2)と双方向の読み書き。
//
// coeff_token / total_zeros / run_before は固定 VLC テーブル、レベルは
// level_prefix + 適応 suffixLength の規則で決まる。すべて決定的なので、
// 構文要素からビット列を一意に再生成できる(規格準拠エンコーダは最短形を
// 使う。非準拠でも採用前のビット一致検証で安全に素通しへ落ちる)。

// ---- coeff_token(Table 9-5) ----

// [nCカテゴリ 0..2][totalCoeff 0..16][trailingOnes 0..3] = 長さ/値。
// カテゴリ: 0: 0≤nC<2, 1: 2≤nC<4, 2: 4≤nC<8。nC≥8 は 6bit FLC。
// chroma DC 用(4:2:0、nC==-1)。[totalCoeff 0..4][t1s 0..3]。
var chromaDCTokenLen = [5][4]uint8{
	{2, 0, 0, 0}, {6, 1, 0, 0}, {6, 6, 3, 0}, {6, 7, 7, 6}, {6, 8, 8, 7},
}
var chromaDCTokenBits = [5][4]uint16{
	{1, 0, 0, 0}, {7, 1, 0, 0}, {4, 6, 1, 0}, {3, 3, 2, 5}, {2, 3, 2, 0},
}

// nCCat は coeff_token テーブル選択(0..2)。nC≥8 は FLC(-1 で表す)。
func nCCat(nC int) int {
	switch {
	case nC < 2:
		return 0
	case nC < 4:
		return 1
	case nC < 8:
		return 2
	}
	return -1 // FLC
}

// readCoeffToken は (totalCoeff, trailingOnes) を読む。nC は -1(chroma DC)
// または 0 以上。
func readCoeffToken(r *h264Reader, nC int) (int, int, error) {
	if nC == -1 {
		return readVLCPair(r, chromaDCTokenLenG[:], chromaDCTokenBitsG[:], 8)
	}
	cat := nCCat(nC)
	if cat < 0 { // 6bit FLC: v = 4*(tc-1)+t1(全 t1 で共通)。
		// あり得ない組 (tc=1,t1=3) の v=3 が tc=0 の番兵に転用されている。
		v, err := r.u(6)
		if err != nil {
			return 0, 0, err
		}
		if v == 3 { // 000011 → totalCoeff=0
			return 0, 0, nil
		}
		tc := int(v>>2) + 1
		t1 := int(v & 3)
		if tc > 16 || t1 > tc {
			return 0, 0, errH264Bits
		}
		return tc, t1, nil
	}
	return readVLCPair(r, coeffTokenLenG[cat][:], coeffTokenBitsG[cat][:], 16)
}

// readVLCPair は (len,bits) 2次元テーブルから最長 maxLen ビットの前方一致で
// (totalCoeff, trailingOnes) を引く。
func readVLCPair(r *h264Reader, lens [][4]uint8, bits [][4]uint16, maxLen int) (int, int, error) {
	var acc uint32
	for n := 1; n <= maxLen; n++ {
		b, err := r.u1()
		if err != nil {
			return 0, 0, err
		}
		acc = acc<<1 | b
		for tc := range lens {
			for t1 := 0; t1 < 4; t1++ {
				if int(lens[tc][t1]) == n && uint32(bits[tc][t1]) == acc {
					return tc, t1, nil
				}
			}
		}
	}
	return 0, 0, errH264Bits
}

// writeCoeffToken は (totalCoeff, trailingOnes) を書く。
func writeCoeffToken(w *h264Writer, nC, tc, t1 int) {
	if nC == -1 {
		w.u(uint32(chromaDCTokenBitsG[tc][t1]), int(chromaDCTokenLenG[tc][t1]))
		return
	}
	cat := nCCat(nC)
	if cat < 0 {
		if tc == 0 {
			w.u(3, 6)
			return
		}
		w.u(uint32(4*(tc-1)+t1), 6)
		return
	}
	w.u(uint32(coeffTokenBitsG[cat][tc][t1]), int(coeffTokenLenG[cat][tc][t1]))
}

// ---- total_zeros(Table 9-7 / 9-8 / 9-9a) ----

// [totalCoeff 1..15][totalZeros] の長さ/値。

// chroma DC 用(4:2:0)。[totalCoeff 1..3][totalZeros]。

func readTotalZeros(r *h264Reader, tc int, chromaDC bool) (int, error) {
	var lens []uint8
	var bits []uint16
	if chromaDC {
		lens, bits = chromaDCTZLenG[tc-1][:], chromaDCTZBitsG[tc-1][:]
	} else {
		lens, bits = totalZerosLenG[tc-1][:], totalZerosBitsG[tc-1][:]
	}
	var acc uint32
	for n := 1; n <= 9; n++ {
		b, err := r.u1()
		if err != nil {
			return 0, err
		}
		acc = acc<<1 | b
		for tz, l := range lens {
			if int(l) == n && uint32(bits[tz]) == acc {
				return tz, nil
			}
		}
	}
	return 0, errH264Bits
}

func writeTotalZeros(w *h264Writer, tc, tz int, chromaDC bool) {
	if chromaDC {
		w.u(uint32(chromaDCTZBitsG[tc-1][tz]), int(chromaDCTZLenG[tc-1][tz]))
	} else {
		w.u(uint32(totalZerosBitsG[tc-1][tz]), int(totalZerosLenG[tc-1][tz]))
	}
}

// ---- run_before(Table 9-10) ----

func readRunBefore(r *h264Reader, zerosLeft int) (int, error) {
	idx := zerosLeft
	if idx > 7 {
		idx = 7
	}
	lens := runBeforeLenG[idx-1][:]
	bits := runBeforeBitsG[idx-1][:]
	var acc uint32
	for n := 1; n <= 11; n++ {
		b, err := r.u1()
		if err != nil {
			return 0, err
		}
		acc = acc<<1 | b
		for run, l := range lens {
			if int(l) == n && uint32(bits[run]) == acc {
				if run > zerosLeft {
					return 0, errH264Bits
				}
				return run, nil
			}
		}
	}
	return 0, errH264Bits
}

func writeRunBefore(w *h264Writer, zerosLeft, run int) {
	idx := zerosLeft
	if idx > 7 {
		idx = 7
	}
	w.u(uint32(runBeforeBitsG[idx-1][run]), int(runBeforeLenG[idx-1][run]))
}

// ---- レベル(9.2.2.1) ----

// readLevel は現在の suffixLength で1レベル(levelCode)を読む。
func readLevelCode(r *h264Reader, suffixLength int) (int, error) {
	prefix := 0
	for {
		b, err := r.u1()
		if err != nil {
			return 0, err
		}
		if b == 1 {
			break
		}
		prefix++
		if prefix > 32 {
			return 0, errH264Bits
		}
	}
	suffixSize := suffixLength
	if prefix == 14 && suffixLength == 0 {
		suffixSize = 4
	} else if prefix >= 15 {
		suffixSize = prefix - 3
	}
	var suffix uint32
	if suffixSize > 0 {
		var err error
		suffix, err = r.u(suffixSize)
		if err != nil {
			return 0, err
		}
	}
	m := prefix
	if m > 15 {
		m = 15
	}
	levelCode := (m << uint(suffixLength)) + int(suffix)
	if prefix >= 15 && suffixLength == 0 {
		levelCode += 15
	}
	if prefix >= 16 {
		levelCode += (1 << uint(prefix-3)) - 4096
	}
	return levelCode, nil
}

// writeLevelCode は levelCode を最短の prefix で書く(規格準拠エンコーダと
// 同じ選択)。書けない値はあり得ない(levelCode は decode 由来)。
func writeLevelCode(w *h264Writer, levelCode, suffixLength int) {
	if suffixLength == 0 {
		if levelCode < 14 {
			w.u(1, levelCode+1) // prefix=levelCode(0の並び)+1
			return
		}
		if levelCode < 30 {
			w.u(1, 15) // prefix=14
			w.u(uint32(levelCode-14), 4)
			return
		}
		// エスケープ: prefix≥15
		rem := levelCode - 30
		prefix := 15
		for rem >= 1<<uint(prefix-3) {
			rem -= 1 << uint(prefix-3)
			prefix++
		}
		w.u(1, prefix+1)
		w.u(uint32(rem), prefix-3)
		return
	}
	if levelCode < 15<<uint(suffixLength) {
		prefix := levelCode >> uint(suffixLength)
		w.u(1, prefix+1)
		w.u(uint32(levelCode)&(1<<uint(suffixLength)-1), suffixLength)
		return
	}
	rem := levelCode - 15<<uint(suffixLength)
	prefix := 15
	for rem >= 1<<uint(prefix-3) {
		rem -= 1 << uint(prefix-3)
		prefix++
	}
	w.u(1, prefix+1)
	w.u(uint32(rem), prefix-3)
}
