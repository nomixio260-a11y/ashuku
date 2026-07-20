package precomp

// AAC-LC raw_data_block の構文走査(parse-only、sink 駆動)。
// 制御フローは復号値(band_type・max_sfb 等)に依存するため、原ビットからの
// 読みも算術からの復号も aacSink 抽象を通す。同じ walker を capture/rebuild
// 両経路が駆動し、lockstep を保つ。対象は SCE/CPE/LFE/FIL/END。DSE/CCE/PCE や
// 非 LC(prediction/gain_control)はフレーム単位で原文退避に落とす。

// aacSink はビット/記号の入出力抽象。
type aacSink interface {
	bits(n int) uint32    // 逐語 n ビット
	spec(cb, ctx int) int // spectral ハフマン記号(ctx=周波数帯)
	sign() int            // 符号ビット
	esc() (int, int)      // cb11 エスケープ: (b=1の連続数, v=b+4 ビット値)
	scf() int             // scalefactor ハフマン記号(0..120)
	align()               // byte_alignment(現在位置からバイト境界まで)
	fail()
	failed() bool
}

// aacWalk は 1 raw_data_block の走査状態。
type aacWalk struct {
	s   aacSink
	sfi int
}

// aacWalkRDB は 1 raw_data_block を走査する。戻り値 false は非対象/失敗。
// alignEnd が >=0 なら byte_alignment 後に消費ビット位置がそこ(ビット)と
// 一致すること(capture 側のみ検証)。
func aacWalkRDB(s aacSink, sfi int) bool {
	w := &aacWalk{s: s, sfi: sfi}
	for {
		et := s.bits(3)
		if s.failed() {
			return false
		}
		if et == 7 { // END
			break
		}
		eid := int(s.bits(4)) // elem_id(FIL では cnt)
		switch et {
		case 0, 3: // SCE, LFE
			w.icsBody(false, &aacIcsState{})
		case 1: // CPE
			w.cpe()
		case 6: // FIL
			w.filWithCnt(eid)
		default: // CCE(2)/DSE(4)/PCE(5)
			s.fail()
		}
		if s.failed() {
			return false
		}
	}
	s.align()
	return !s.failed()
}

type aacIcsState struct {
	windowSeq  int
	maxSfb     int
	numWindows int
	numGroups  int
	groupLen   [8]int
	swb        []uint16
}

// filWithCnt は FIL 要素を逐語で消費する(cnt=elem_id、15 で拡張)。
func (w *aacWalk) filWithCnt(cnt int) {
	s := w.s
	if cnt == 15 {
		esc := int(s.bits(8))
		cnt = 15 + esc - 1
	}
	for i := 0; i < cnt && !s.failed(); i++ {
		s.bits(8)
	}
}

func (w *aacWalk) cpe() {
	s := w.s
	common := s.bits(1) == 1
	var ics aacIcsState
	if common {
		w.icsInfo(&ics)
		if s.failed() {
			return
		}
		msPresent := s.bits(2)
		if msPresent == 3 {
			s.fail()
			return
		}
		if msPresent == 1 {
			n := ics.numGroups * ics.maxSfb
			for i := 0; i < n; i++ {
				s.bits(1)
			}
		}
	}
	w.icsBody(common, &ics)
	if s.failed() {
		return
	}
	w.icsBody(common, &ics)
}

// icsBody は global_gain 以降を走査する。
func (w *aacWalk) icsBody(common bool, ics *aacIcsState) {
	s := w.s
	s.bits(8) // global_gain
	if !common {
		w.icsInfo(ics)
		if s.failed() {
			return
		}
	}
	bandType := make([]int, ics.numGroups*ics.maxSfb)
	w.sectionData(ics, bandType)
	if s.failed() {
		return
	}
	w.scaleFactors(ics, bandType)
	if s.failed() {
		return
	}
	if s.bits(1) == 1 { // pulse_data_present
		if ics.windowSeq == 2 {
			s.fail()
			return
		}
		w.pulseData()
	}
	if s.bits(1) == 1 { // tns_data_present
		w.tnsData(ics)
	}
	if s.bits(1) == 1 { // gain_control_data_present(LC は 0)
		s.fail()
		return
	}
	if s.failed() {
		return
	}
	w.spectralData(ics, bandType)
}

func (w *aacWalk) icsInfo(ics *aacIcsState) {
	s := w.s
	s.bits(1) // reserved
	ics.windowSeq = int(s.bits(2))
	s.bits(1) // window_shape
	ics.numGroups = 1
	ics.groupLen[0] = 1
	if ics.windowSeq == 2 {
		ics.maxSfb = int(s.bits(4))
		for i := 0; i < 7; i++ {
			if s.bits(1) == 1 {
				ics.groupLen[ics.numGroups-1]++
			} else {
				ics.numGroups++
				ics.groupLen[ics.numGroups-1] = 1
			}
		}
		ics.numWindows = 8
		if w.sfi >= len(aacSwbOffset128) {
			s.fail()
			return
		}
		ics.swb = aacSwbOffset128[w.sfi]
	} else {
		ics.maxSfb = int(s.bits(6))
		ics.numWindows = 1
		if w.sfi >= len(aacSwbOffset1024) {
			s.fail()
			return
		}
		ics.swb = aacSwbOffset1024[w.sfi]
		if s.bits(1) == 1 { // predictor_data_present(LC は 0)
			s.fail()
			return
		}
	}
	if s.failed() || ics.maxSfb > len(ics.swb)-1 {
		s.fail()
	}
}

func (w *aacWalk) sectionData(ics *aacIcsState, bandType []int) {
	s := w.s
	bits := 5
	if ics.windowSeq == 2 {
		bits = 3
	}
	esc := (1 << bits) - 1
	for g := 0; g < ics.numGroups; g++ {
		k := 0
		for k < ics.maxSfb {
			sbt := int(s.bits(4))
			if sbt == 12 {
				s.fail()
				return
			}
			end := k
			for {
				incr := int(s.bits(bits))
				end += incr
				if end > ics.maxSfb {
					s.fail()
					return
				}
				if incr != esc {
					break
				}
			}
			for ; k < end; k++ {
				bandType[g*ics.maxSfb+k] = sbt
			}
			if s.failed() {
				return
			}
		}
	}
}

func (w *aacWalk) scaleFactors(ics *aacIcsState, bandType []int) {
	s := w.s
	noiseFlag := 1
	for g := 0; g < ics.numGroups; g++ {
		for sfb := 0; sfb < ics.maxSfb; sfb++ {
			bt := bandType[g*ics.maxSfb+sfb]
			switch {
			case bt == 0:
			case bt == 14 || bt == 15:
				s.scf()
			case bt == 13:
				if noiseFlag > 0 {
					noiseFlag--
					s.bits(9)
				} else {
					s.scf()
				}
			default:
				s.scf()
			}
			if s.failed() {
				return
			}
		}
	}
}

func (w *aacWalk) pulseData() {
	s := w.s
	num := int(s.bits(2)) + 1
	s.bits(6)
	s.bits(5)
	s.bits(4)
	for i := 1; i < num; i++ {
		s.bits(5)
		s.bits(4)
	}
}

func (w *aacWalk) tnsData(ics *aacIcsState) {
	s := w.s
	is8 := 0
	if ics.windowSeq == 2 {
		is8 = 1
	}
	for win := 0; win < ics.numWindows; win++ {
		nFilt := int(s.bits(2 - is8))
		if nFilt == 0 {
			continue
		}
		coefRes := int(s.bits(1))
		for f := 0; f < nFilt; f++ {
			s.bits(6 - 2*is8)
			order := int(s.bits(5 - 2*is8))
			if order > 0 {
				s.bits(1)
				coefCompress := int(s.bits(1))
				coefLen := coefRes + 3 - coefCompress
				for i := 0; i < order; i++ {
					s.bits(coefLen)
				}
			}
		}
		if s.failed() {
			return
		}
	}
}

func (w *aacWalk) spectralData(ics *aacIcsState, bandType []int) {
	s := w.s
	for g := 0; g < ics.numGroups; g++ {
		gl := ics.groupLen[g]
		for sfb := 0; sfb < ics.maxSfb; sfb++ {
			cb := bandType[g*ics.maxSfb+sfb]
			if cb == 0 || cb == 13 || cb == 14 || cb == 15 {
				continue
			}
			if cb < 1 || cb > 11 {
				s.fail()
				return
			}
			offLen := int(ics.swb[sfb+1] - ics.swb[sfb])
			t := &aacSpecTables[cb]
			ctx := 0
			if sfb*3 >= ics.maxSfb { // 低域(トーン主体)/高域で分ける
				ctx = 1
			}
			for grp := 0; grp < gl; grp++ {
				n := offLen
				for n > 0 {
					w.specTuple(cb, ctx, t)
					if s.failed() {
						return
					}
					n -= t.Dim
				}
			}
		}
	}
}

func (w *aacWalk) specTuple(cb, ctx int, t *aacHuffTab) {
	s := w.s
	sym := s.spec(cb, ctx)
	if s.failed() {
		return
	}
	if !t.Unsigned {
		return
	}
	mags := aacSpecMags(cb, sym)
	for _, m := range mags {
		if m != 0 {
			s.sign()
		}
	}
	if cb == 11 {
		for _, m := range mags {
			if m == 16 {
				s.esc()
				if s.failed() {
					return
				}
			}
		}
	}
}
