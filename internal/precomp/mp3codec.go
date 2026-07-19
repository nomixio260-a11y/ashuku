package precomp

// MP3(MPEG-1/2/2.5 Audio Layer III)の可逆再圧縮。
//
// MP3 のスペクトルデータは固定ハフマン表(32本+count1 2本)で符号化されて
// おり、JPEG/CAVLC と同じ「VLC→文脈適応算術」の載せ替えが効く。
//
// 設計:
//  - フレームヘッダ+CRC+サイド情報は原文のまま骨格(skeleton)に保持。
//  - main_data はビットリザーバ(フレーム跨ぎの共有プール)なので、全フレームの
//    main_data バイトを1本のプールに連結し、サイド情報(main_data_begin /
//    part2_3_length)に従ってグラニュール単位で構文解析する。
//  - スケールファクタ+ハフマン記号を文脈算術で再符号化。**グラニュールごとに
//    正準ハフマン再符号化でビット一致を検証**し、一致しないグラニュール
//    (境界跨ぎ等の非準拠ストリーム)はフラグ付きで原文ビットを素通しする。
//    → どんな入力でも可逆性が保たれ、通常グラニュールは縮む。
//  - グラニュール間の隙間(アンシラリ/スタッフィング)も原文ビットで保存。
//  - 再構成はプールをビット単位で再生成し、骨格のフレームサイズに従って
//    main_data 領域へ切り戻す。ID3v2/v1 タグ等の前後は骨格に原文保存。

import "errors"

var errMP3 = errors.New("mp3 解析エラー")

// IsMP3 は MP3 らしいか(ID3v2 タグ or フレーム同期)を判定する。
func IsMP3(head []byte) bool {
	if len(head) >= 3 && head[0] == 'I' && head[1] == 'D' && head[2] == '3' {
		return true
	}
	return len(head) >= 2 && head[0] == 0xFF && head[1]&0xE0 == 0xE0 && (head[1]>>1)&3 == 1
}

// mp3FrameHdr はヘッダ解析結果。
type mp3FrameHdr struct {
	lsf, mpeg25 bool
	crc         bool
	bitrateIdx  int
	srIdx       int // 0..8 の再写像済みインデックス
	sampleRate  int
	padding     int
	mode        int // 0=stereo 1=joint 2=dual 3=mono
	modeExt     int
	frameSize   int
	sideBytes   int
	nChannels   int
}

var mp3Bitrates = [2][15]int{
	{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320}, // MPEG-1 L3
	{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},     // MPEG-2/2.5 L3
}
var mp3SampleRates = [3][3]int{
	{44100, 48000, 32000}, // MPEG-1
	{22050, 24000, 16000}, // MPEG-2
	{11025, 12000, 8000},  // MPEG-2.5
}

// parseMP3Header は4バイトの Layer III ヘッダを解析する(非対応は nil)。
func parseMP3Header(b []byte) *mp3FrameHdr {
	if len(b) < 4 || b[0] != 0xFF || b[1]&0xE0 != 0xE0 {
		return nil
	}
	verBits := (b[1] >> 3) & 3 // 0=2.5, 2=MPEG2, 3=MPEG1
	layer := (b[1] >> 1) & 3   // 1 = Layer III
	if verBits == 1 || layer != 1 {
		return nil
	}
	h := &mp3FrameHdr{}
	h.lsf = verBits != 3
	h.mpeg25 = verBits == 0
	h.crc = b[1]&1 == 0
	h.bitrateIdx = int(b[2] >> 4)
	srRaw := int(b[2]>>2) & 3
	if h.bitrateIdx == 0 || h.bitrateIdx == 15 || srRaw == 3 {
		return nil // free format / 不正は対象外
	}
	h.padding = int(b[2]>>1) & 1
	h.mode = int(b[3] >> 6)
	h.modeExt = int(b[3]>>4) & 3
	verRow := 0
	if h.mpeg25 {
		verRow = 2
	} else if h.lsf {
		verRow = 1
	}
	h.sampleRate = mp3SampleRates[verRow][srRaw]
	h.srIdx = srRaw + 3*verRow
	brRow := 0
	if h.lsf {
		brRow = 1
	}
	kbps := mp3Bitrates[brRow][h.bitrateIdx]
	shift := 0
	if h.lsf {
		shift = 1
	}
	h.frameSize = 144000*kbps/(h.sampleRate<<shift) + h.padding
	h.nChannels = 2
	if h.mode == 3 {
		h.nChannels = 1
	}
	if h.lsf {
		h.sideBytes = 9
		if h.nChannels == 2 {
			h.sideBytes = 17
		}
	} else {
		h.sideBytes = 17
		if h.nChannels == 2 {
			h.sideBytes = 32
		}
	}
	return h
}

// mp3Granule は1グラニュール×1チャネルのサイド情報。
type mp3Granule struct {
	part23Len  int
	bigValues  int
	globalGain int
	scaleComp  int
	blockSplit bool
	blockType  int
	mixed      bool
	tableSel   [3]int
	subblockG  [3]int
	region0    int
	region1    int
	preflag    int
	scaleScale int
	count1Sel  int
}

// mp3SideInfo は1フレームのサイド情報。
type mp3SideInfo struct {
	mainDataBegin int
	scfsi         [2][4]int
	gr            [2][2]mp3Granule // [granule][channel]
	nGranules     int
}

// parseMP3SideInfo はサイド情報を解析する。
func parseMP3SideInfo(r *h264Reader, h *mp3FrameHdr) (*mp3SideInfo, bool) {
	si := &mp3SideInfo{}
	si.nGranules = 2
	if h.lsf {
		si.nGranules = 1
	}
	nbits := 9
	if h.lsf {
		nbits = 8
	}
	v, err := r.u(nbits)
	if err != nil {
		return nil, false
	}
	si.mainDataBegin = int(v)
	// private_bits
	priv := 0
	if h.lsf {
		if h.nChannels == 1 {
			priv = 1
		} else {
			priv = 2
		}
	} else {
		if h.nChannels == 1 {
			priv = 5
		} else {
			priv = 3
		}
	}
	if _, err := r.u(priv); err != nil {
		return nil, false
	}
	if !h.lsf {
		for ch := 0; ch < h.nChannels; ch++ {
			for i := 0; i < 4; i++ {
				b, err := r.u1()
				if err != nil {
					return nil, false
				}
				si.scfsi[ch][i] = int(b)
			}
		}
	}
	for g := 0; g < si.nGranules; g++ {
		for ch := 0; ch < h.nChannels; ch++ {
			gr := &si.gr[g][ch]
			if v, err = r.u(12); err != nil {
				return nil, false
			}
			gr.part23Len = int(v)
			if v, err = r.u(9); err != nil {
				return nil, false
			}
			gr.bigValues = int(v)
			if gr.bigValues > 288 {
				return nil, false
			}
			if v, err = r.u(8); err != nil {
				return nil, false
			}
			gr.globalGain = int(v)
			scBits := 4
			if h.lsf {
				scBits = 9
			}
			if v, err = r.u(scBits); err != nil {
				return nil, false
			}
			gr.scaleComp = int(v)
			b, err := r.u1()
			if err != nil {
				return nil, false
			}
			gr.blockSplit = b == 1
			if gr.blockSplit {
				if v, err = r.u(2); err != nil {
					return nil, false
				}
				gr.blockType = int(v)
				if gr.blockType == 0 {
					return nil, false
				}
				b, err := r.u1()
				if err != nil {
					return nil, false
				}
				gr.mixed = b == 1
				for i := 0; i < 2; i++ {
					if v, err = r.u(5); err != nil {
						return nil, false
					}
					gr.tableSel[i] = int(v)
				}
				for i := 0; i < 3; i++ {
					if v, err = r.u(3); err != nil {
						return nil, false
					}
					gr.subblockG[i] = int(v)
				}
				// FFmpeg 準拠の region 既定(mpegaudiodec_common.c)
				if gr.blockType == 2 && !gr.mixed {
					gr.region0 = 8
				} else {
					gr.region0 = 7
				}
				gr.region1 = 20 - gr.region0 // = 全帯域(region2 なし)
			} else {
				for i := 0; i < 3; i++ {
					if v, err = r.u(5); err != nil {
						return nil, false
					}
					gr.tableSel[i] = int(v)
				}
				if v, err = r.u(4); err != nil {
					return nil, false
				}
				gr.region0 = int(v)
				if v, err = r.u(3); err != nil {
					return nil, false
				}
				gr.region1 = int(v)
			}
			if !h.lsf {
				b, err := r.u1()
				if err != nil {
					return nil, false
				}
				gr.preflag = int(b)
			}
			b, err = r.u1()
			if err != nil {
				return nil, false
			}
			gr.scaleScale = int(b)
			b, err = r.u1()
			if err != nil {
				return nil, false
			}
			gr.count1Sel = int(b)
		}
	}
	return si, true
}

// mp3Frame は1フレームの解析結果。
type mp3Frame struct {
	off      int // ファイル内オフセット
	hdr      *mp3FrameHdr
	si       *mp3SideInfo
	dataOff  int // main_data のファイル内開始
	dataLen  int
	poolBase int // このフレームの main_data がプール内で始まる位置(バイト)
}

// mp3Walk はファイルを走査してフレーム列とプールを作る。
// 戻り値: prefix 長(ID3v2 等)、フレーム列、プール、suffix 長、ok。
func mp3Walk(orig []byte) (int, []*mp3Frame, []byte, int, bool) {
	pos := 0
	// ID3v2
	if len(orig) >= 10 && orig[0] == 'I' && orig[1] == 'D' && orig[2] == '3' {
		sz := int(orig[6]&0x7F)<<21 | int(orig[7]&0x7F)<<14 | int(orig[8]&0x7F)<<7 | int(orig[9]&0x7F)
		pos = 10 + sz
		if pos >= len(orig) {
			return 0, nil, nil, 0, false
		}
	}
	prefix := pos
	var frames []*mp3Frame
	var pool []byte
	for pos+4 <= len(orig) {
		h := parseMP3Header(orig[pos:])
		if h == nil {
			break
		}
		if pos+h.frameSize > len(orig) {
			break // 末尾の欠けフレームは suffix 扱い
		}
		hdrCrc := 4
		if h.crc {
			hdrCrc = 6
		}
		dataOff := pos + hdrCrc + h.sideBytes
		dataLen := h.frameSize - hdrCrc - h.sideBytes
		if dataLen < 0 {
			return 0, nil, nil, 0, false
		}
		sr := &h264Reader{b: orig[pos+hdrCrc:]}
		si, ok := parseMP3SideInfo(sr, h)
		if !ok {
			return 0, nil, nil, 0, false
		}
		f := &mp3Frame{off: pos, hdr: h, si: si, dataOff: dataOff, dataLen: dataLen, poolBase: len(pool)}
		pool = append(pool, orig[dataOff:dataOff+dataLen]...)
		frames = append(frames, f)
		pos += h.frameSize
	}
	if len(frames) == 0 {
		return 0, nil, nil, 0, false
	}
	return prefix, frames, pool, len(orig) - pos, true
}
