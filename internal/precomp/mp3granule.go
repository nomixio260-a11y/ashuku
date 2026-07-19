package precomp

// MP3 グラニュール(part2: スケールファクタ + part3: ハフマン)の記号走査。
//
// mp3Sink 抽象で「capture(記号→算術)」「rebuild(算術→ビット再生成)」の
// 2経路を lockstep に保つ(CABAC の cabacSink と同じ設計)。読み経路では
// src.r から復号した記号を sink に通し、書き経路では sink から記号を得て
// src.w へ正準ハフマン符号を書く。

// mp3Sink は記号入出力の抽象。読み経路では引数の値を記録して同じ値を返し、
// 書き経路では記録済みの値を返す。
type mp3Sink interface {
	sf(slen, idx, v int) int
	huff(t, pos, sym int) int // pos = 係数開始インデックス(0..575)
	quad(tsel, pos, sym int) int
	cont(v int) int // count1 継続フラグ(1=quad あり)
	raw(n int, v uint32) uint32
	granule() // グラニュール境界(文脈リセット)
	fail()
	failed() bool
}

// mp3HuffDec は VLC id ごとの復号マップ((bits<<16)|code → symbol)。
var mp3HuffDec [32]map[uint32]int
var mp3QuadDec [2]map[uint32]int

func init() {
	for t := 1; t < 32; t++ {
		ht := &mp3HuffTables[t]
		if len(ht.Codes) == 0 {
			continue
		}
		m := make(map[uint32]int, len(ht.Codes))
		for s, c := range ht.Codes {
			b := ht.Bits[s]
			if b == 0 {
				continue
			}
			m[uint32(b)<<16|uint32(c)] = s
		}
		mp3HuffDec[t] = m
	}
	for t := 0; t < 2; t++ {
		m := make(map[uint32]int, 16)
		for s := 0; s < 16; s++ {
			m[uint32(mp3QuadBits[t][s])<<16|uint32(mp3QuadCodes[t][s])] = s
		}
		mp3QuadDec[t] = m
	}
}

// mp3BitSrc は読み(pool)または書き(rebuild)のビットカーソル。
type mp3BitSrc struct {
	r   *h264Reader
	w   *h264Writer
	end int // 読み: このグラニュールの終端ビット位置
}

// mp3ReadHuffSym は読み経路で1記号を復号する(limit を跨いだら失敗)。
func mp3ReadHuffSym(r *h264Reader, dec map[uint32]int, limit int) (int, bool) {
	code := uint32(0)
	for n := 1; n <= 24; n++ {
		if r.pos >= limit {
			return 0, false
		}
		bit, err := r.u1()
		if err != nil {
			return 0, false
		}
		code = code<<1 | bit
		if s, ok := dec[uint32(n)<<16|code]; ok {
			return s, true
		}
	}
	return 0, false
}

// mp3LsfSlen は LSF の scalefac_compress → (slen[4], tindex2)。
// FFmpeg lsf_sf_expand(SPLIT は n による剰余/除算)と同一。
func mp3LsfSlen(scaleComp int, intensity bool) ([4]int, int) {
	sf := scaleComp
	var slen [4]int
	var t2 int
	div := func(n int) int {
		m := sf % n
		sf /= n
		return m
	}
	if intensity {
		sf >>= 1
		switch {
		case sf < 180:
			slen[3] = 0
			slen[2] = div(6)
			slen[1] = div(6)
			slen[0] = sf
			t2 = 3
		case sf < 244:
			sf -= 180
			slen[3] = 0
			slen[2] = div(4)
			slen[1] = div(4)
			slen[0] = sf
			t2 = 4
		default:
			sf -= 244
			slen[3] = 0
			slen[2] = 0
			slen[1] = div(3)
			slen[0] = sf
			t2 = 5
		}
	} else {
		switch {
		case sf < 400:
			slen[3] = div(4)
			slen[2] = div(4)
			slen[1] = div(5)
			slen[0] = sf
			t2 = 0
		case sf < 500:
			sf -= 400
			slen[3] = 0
			slen[2] = div(4)
			slen[1] = div(5)
			slen[0] = sf
			t2 = 1
		default:
			sf -= 500
			slen[3] = 0
			slen[2] = 0
			slen[1] = div(3)
			slen[0] = sf
			t2 = 2
		}
	}
	return slen, t2
}

// mp3ScanGranule は1グラニュール×1チャネルの part2+part3 を走査する。
// 読み経路: src.r の現在位置から src.end まで。書き経路: src.w へ再生成
// (末尾スタッフィングは呼び出し側が扱う)。
func mp3ScanGranule(sink mp3Sink, src *mp3BitSrc, h *mp3FrameHdr, si *mp3SideInfo, g, ch int) bool {
	gr := &si.gr[g][ch]
	sink.granule()
	sfIdx := 0
	readSF := func(slen int) bool {
		ok := mp3SF(sink, src, slen, sfIdx)
		sfIdx++
		return ok
	}
	// --- part2: スケールファクタ ---
	if !h.lsf {
		slen1 := int(mp3SlenTable[0][gr.scaleComp])
		slen2 := int(mp3SlenTable[1][gr.scaleComp])
		if gr.blockSplit && gr.blockType == 2 {
			n := 18
			if gr.mixed {
				n = 17
			}
			for i := 0; i < n; i++ {
				if !readSF(slen1) {
					return false
				}
			}
			for i := 0; i < 18; i++ {
				if !readSF(slen2) {
					return false
				}
			}
		} else {
			slens := [4]int{slen1, slen1, slen2, slen2}
			for k := 0; k < 4; k++ {
				if g == 1 && si.scfsi[ch][k] == 1 {
					continue // granule0 と共有(ビットなし)
				}
				n := 5
				if k == 0 {
					n = 6
				}
				for i := 0; i < n; i++ {
					if !readSF(slens[k]) {
						return false
					}
				}
			}
		}
	} else {
		intensity := h.modeExt&1 != 0 && ch == 1
		slen, t2 := mp3LsfSlen(gr.scaleComp, intensity)
		tindex := 0
		if gr.blockSplit && gr.blockType == 2 {
			if gr.mixed {
				tindex = 2
			} else {
				tindex = 1
			}
		}
		for k := 0; k < 4; k++ {
			n := int(mp3LsfNsfTable[t2][tindex][k])
			for i := 0; i < n; i++ {
				if !readSF(slen[k]) {
					return false
				}
			}
		}
	}
	// --- part3: ハフマン ---
	return mp3ScanHuffman(sink, src, h, gr)
}

// mp3SF は1スケールファクタ(slen ビット)を通す。
func mp3SF(sink mp3Sink, src *mp3BitSrc, slen, idx int) bool {
	if slen == 0 {
		return true
	}
	if src.r != nil {
		if src.r.pos+slen > src.end {
			sink.fail()
			return false
		}
		v, err := src.r.u(slen)
		if err != nil {
			sink.fail()
			return false
		}
		sink.sf(slen, idx, int(v))
	} else {
		v := sink.sf(slen, idx, 0)
		src.w.u(uint32(v), slen)
	}
	return !sink.failed()
}

// mp3Regions は big_values の3領域のペア数(FFmpeg init_*_region +
// region_offset2size と同一)。
func mp3Regions(h *mp3FrameHdr, gr *mp3Granule) [3]int {
	var bound [3]int
	if gr.blockSplit {
		if gr.blockType == 2 {
			if h.srIdx != 8 {
				bound[0] = 36 / 2
			} else {
				bound[0] = 72 / 2
			}
		} else {
			if h.srIdx <= 2 {
				bound[0] = 36 / 2
			} else if h.srIdx != 8 {
				bound[0] = 54 / 2
			} else {
				bound[0] = 108 / 2
			}
		}
		bound[1] = 576 / 2
	} else {
		bound[0] = int(mp3BandIndexLong[h.srIdx][gr.region0+1])
		l := gr.region0 + gr.region1 + 2
		if l > 22 {
			l = 22
		}
		bound[1] = int(mp3BandIndexLong[h.srIdx][l])
	}
	bound[2] = 576 / 2
	var size [3]int
	j := 0
	for i := 0; i < 3; i++ {
		k := bound[i]
		if k > gr.bigValues {
			k = gr.bigValues
		}
		size[i] = k - j
		if size[i] < 0 {
			size[i] = 0
		} else {
			j = k
		}
	}
	return size
}

// mp3ScanHuffman は part3(big_values + count1)を走査する。
func mp3ScanHuffman(sink mp3Sink, src *mp3BitSrc, h *mp3FrameHdr, gr *mp3Granule) bool {
	regions := mp3Regions(h, gr)
	sIndex := 0
	for ri := 0; ri < 3; ri++ {
		t := gr.tableSel[ri]
		ht := &mp3HuffTables[t]
		empty := len(ht.Codes) == 0
		for p := 0; p < regions[ri]; p++ {
			sIndex += 2
			if empty {
				continue // 全ゼロ・ビットなし
			}
			var sym int
			if src.r != nil {
				s, ok := mp3ReadHuffSym(src.r, mp3HuffDec[t], src.end)
				if !ok {
					sink.fail()
					return false
				}
				sym = sink.huff(t, sIndex, s)
			} else {
				sym = sink.huff(t, sIndex, 0)
				if sym < 0 || sym >= len(ht.Codes) {
					sink.fail()
					return false
				}
				src.w.u(uint32(ht.Codes[sym]), int(ht.Bits[sym]))
			}
			if sink.failed() {
				return false
			}
			x, y := sym/ht.YSize, sym%ht.YSize
			for _, mag := range [2]int{x, y} {
				if mag == 15 && ht.LinBits > 0 {
					if !mp3Raw(sink, src, ht.LinBits) {
						return false
					}
				}
				if mag != 0 {
					if !mp3Raw(sink, src, 1) {
						return false
					}
				}
			}
		}
	}
	// count1: 継続フラグを sink 経由で対称に扱う
	for sIndex <= 572 {
		if src.r != nil {
			if src.r.pos >= src.end {
				sink.cont(0)
				break
			}
			sink.cont(1)
			s, ok := mp3ReadHuffSym(src.r, mp3QuadDec[gr.count1Sel], src.end)
			if !ok {
				sink.fail() // 境界跨ぎ(非準拠)等 → このグラニュールは素通しへ
				return false
			}
			sym := sink.quad(gr.count1Sel, sIndex, s)
			for b := 3; b >= 0; b-- {
				if sym&(1<<uint(b)) != 0 {
					if !mp3Raw(sink, src, 1) {
						return false
					}
				}
			}
		} else {
			if sink.cont(0) == 0 {
				break
			}
			sym := sink.quad(gr.count1Sel, sIndex, 0)
			src.w.u(uint32(mp3QuadCodes[gr.count1Sel][sym]), int(mp3QuadBits[gr.count1Sel][sym]))
			for b := 3; b >= 0; b-- {
				if sym&(1<<uint(b)) != 0 {
					if !mp3Raw(sink, src, 1) {
						return false
					}
				}
			}
		}
		sIndex += 4
		if sink.failed() {
			return false
		}
	}
	return !sink.failed()
}

// mp3Raw は n ビットの生データを通す。
func mp3Raw(sink mp3Sink, src *mp3BitSrc, n int) bool {
	if src.r != nil {
		if src.r.pos+n > src.end {
			sink.fail()
			return false
		}
		v, err := src.r.u(n)
		if err != nil {
			sink.fail()
			return false
		}
		sink.raw(n, v)
	} else {
		v := sink.raw(n, 0)
		src.w.u(v, n)
	}
	return !sink.failed()
}
