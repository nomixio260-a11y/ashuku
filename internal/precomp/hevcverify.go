package precomp

// HEVC 走査の検証層: 復号のみのシンクで全スライスを走査し、
// スライス末尾でバイト消費位置が RBSP 長と一致することを確認する
// (遅延デシンク・オラクル)。

// hevcDecSink は検証用シンク(復号のみ)。WPP 用の文脈退避も持つ。
type hevcDecSink struct {
	d     *cabacDecoder
	st    [hevcNumContexts]uint8
	saved [hevcNumContexts]uint8
}

func (s *hevcDecSink) decision(ctx int) int { return s.d.decodeDecision(&s.st[ctx]) }
func (s *hevcDecSink) bypass() int          { return s.d.decodeBypass() }
func (s *hevcDecSink) terminate() int       { return s.d.decodeTerminate() }
func (s *hevcDecSink) failed() bool         { return s.d.err }

// intraPCM は PCM 生バイトの読み飛ばし/WPP 行頭の整列再初期化(n=0)。
// H.264 側と同じ較正: 消費位置 C = pos-9 を切り上げ整列する。
func (s *hevcDecSink) intraPCM(nBytes int) bool {
	c := s.d.r.pos - 9
	start := (c + 7) &^ 7
	end := start + nBytes*8
	if end > len(s.d.r.b)*8 {
		s.d.err = true
		return false
	}
	s.d.reinitAt(end)
	return !s.d.err
}

func (s *hevcDecSink) initCtx(sliceQP, initType int) { hevcInitStates(&s.st, sliceQP, initType) }
func (s *hevcDecSink) saveCtx()                      { s.saved = s.st }
func (s *hevcDecSink) loadCtx()                      { s.st = s.saved }

// reinitEngine はサブストリーム境界(body 基準バイト位置)で復号器を
// 再初期化する。
func (s *hevcDecSink) reinitEngine(bytePos int) bool {
	if bytePos < 0 || bytePos*8 >= len(s.d.r.b)*8 {
		s.d.err = true
		return false
	}
	s.d.reinitAt(bytePos * 8)
	return !s.d.err
}

// hevcEntryOffsetsRBSP は entry_point_offset(エスケープ領域のバイト長)を
// RBSP 領域のバイト長へ変換する。nal は NAL 全体(エスケープ済み)。
// dataStartRBSP は RBSP 上のスライスデータ開始位置(NAL ヘッダ 2B 含む)。
func hevcEntryOffsetsRBSP(nal []byte, dataStartRBSP int, offs []int) ([]int, bool) {
	if len(offs) == 0 {
		return nil, true
	}
	var esc3 []int // emulation 0x03 のエスケープ領域での位置
	escStart := -1
	i, out := 0, 0
	for i < len(nal) {
		if escStart < 0 && out == dataStartRBSP {
			escStart = i
		}
		if i+2 < len(nal) && nal[i] == 0 && nal[i+1] == 0 && nal[i+2] == 3 {
			if escStart < 0 && out+1 == dataStartRBSP {
				escStart = i + 1 // 00 00 対の 2 バイト目が開始位置
			}
			out += 2
			esc3 = append(esc3, i+2)
			i += 3
			continue
		}
		out++
		i++
	}
	if escStart < 0 && out == dataStartRBSP {
		escStart = i
	}
	if escStart < 0 {
		return nil, false
	}
	res := make([]int, len(offs))
	e := escStart
	ri := 0
	for ri < len(esc3) && esc3[ri] < e {
		ri++
	}
	for k, off := range offs {
		e2 := e + off
		cnt := 0
		for ri < len(esc3) && esc3[ri] < e2 {
			cnt++
			ri++
		}
		res[k] = off - cnt
		e = e2
	}
	if e > len(nal)+2 {
		return nil, false
	}
	return res, true
}

// hevcVerifyResult は検証走査の結果。
type hevcVerifyResult struct {
	slices int
	ctus   int // 参考値(未使用なら 0)
}

// hevcVerifyStream は Annex B の HEVC ストリーム全体を検証走査する。
// 失敗時は (どの NAL で失敗したか, false)。
func hevcVerifyStream(data []byte) (hevcVerifyResult, int, bool) {
	res := hevcVerifyResult{}
	nals, ok := splitAnnexB(data)
	if !ok {
		return res, -1, false
	}
	spsMap := map[int]*hevcSPS{}
	ppsMap := map[int]*hevcPPS{}
	var pic *hevcPicState
	for i, nal := range nals {
		typ := hevcNALType(nal.data)
		switch {
		case typ == hevcNALSPS:
			rbsp := unescapeRBSP(nal.data)
			s, ok := parseHEVCSPS(rbsp[2:])
			if !ok {
				return res, i, false
			}
			spsMap[s.spsID] = s
		case typ == hevcNALPPS:
			rbsp := unescapeRBSP(nal.data)
			p, ok := parseHEVCPPS(rbsp[2:])
			if !ok {
				return res, i, false
			}
			ppsMap[p.ppsID] = p
		case hevcIsSlice(typ):
			rbsp := unescapeRBSP(nal.data)
			body := rbsp[2:]
			ppsID, ok := hevcPeekSlicePPSID(body, typ)
			if !ok {
				return res, i, false
			}
			pps := ppsMap[ppsID]
			if pps == nil {
				return res, i, false
			}
			sps := spsMap[pps.spsID]
			if sps == nil {
				return res, i, false
			}
			if sps.chromaFormatIDC != 1 {
				return res, i, false // v1: 4:2:0 のみ
			}
			r := &h264Reader{b: body}
			sl, ok := parseHEVCSliceHeader(r, sps, pps, typ)
			if !ok {
				return res, i, false
			}
			if len(sl.entryOffsets) > 0 {
				conv, ok := hevcEntryOffsetsRBSP(nal.data, 2+sl.headerBits/8, sl.entryOffsets)
				if !ok {
					return res, i, false
				}
				sl.entryOffsets = conv
			}
			if sl.firstSlice || pic == nil || pic.sps != sps {
				pic = newHEVCPicState(sps, pps)
			}
			sink := &hevcDecSink{d: newCabacDecoder(r)}
			if !hevcWalkSliceData(sink, sink, pic, sl) {
				return res, i, false
			}
			// 遅延デシンク・オラクル: 最終 terminate 時点の読み位置が
			// RBSP 末尾近傍(フラッシュ+トレーリング+ゼロワード ≤6B)に
			// あること。バイト厳密な検証は正準再符号化(capture)側で行う。
			endApprox := r.pos / 8
			if endApprox > len(body) || len(body)-endApprox > 6 {
				return res, i, false
			}
			res.slices++
		}
	}
	if res.slices == 0 {
		return res, -1, false
	}
	return res, -1, true
}

// hevcPeekSlicePPSID はスライスヘッダ先頭から pps_id だけを読む。
func hevcPeekSlicePPSID(body []byte, nalType int) (int, bool) {
	r := &h264Reader{b: body}
	if _, err := r.u1(); err != nil { // first_slice_segment_in_pic_flag
		return 0, false
	}
	if hevcIsIRAP(nalType) {
		if _, err := r.u1(); err != nil {
			return 0, false
		}
	}
	v, err := r.ue()
	if err != nil || v > 63 {
		return 0, false
	}
	return int(v), true
}
