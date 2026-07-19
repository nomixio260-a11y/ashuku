package precomp

// H.264 トランスコードのコンテナ非依存エンジン。
//
// Annex B(生ストリーム)と MP4(長さ前置サンプル)のどちらも、中身は
// 同じ NAL 列(エミュレーション防止込みの EBSP)なので、NAL 単位の
// capture / rebuild をここに集約し、コンテナ層(h264codec.go / mp4.go)は
// NAL の切り出しとスケルトン管理だけを行う。

// h264Capture は capture(元ビット→算術)側のエンジン状態。
type h264Capture struct {
	spsMap map[int]*h264SPS
	ppsMap map[int]*h264PPS
	enc    *rangeEncoder
	model  *h264Model
	nz     *h264NZ
	coded  int
}

func newH264Capture() *h264Capture {
	return &h264Capture{
		spsMap: map[int]*h264SPS{},
		ppsMap: map[int]*h264PPS{},
		enc:    newRangeEncoder(),
		model:  newH264Model(),
	}
}

// observePS は SPS/PPS NAL(EBSP、NALヘッダ込み)をパラメータ表に取り込む。
func (c *h264Capture) observePS(nalData []byte) {
	if len(nalData) < 2 {
		return
	}
	typ := int(nalData[0] & 0x1F)
	if typ != 7 && typ != 8 {
		return
	}
	rbsp := unescapeRBSP(nalData)
	if typ == 7 {
		if s, ok := parseSPS(rbsp[1:]); ok {
			c.spsMap[spsIDOf(rbsp[1:])] = s
		}
	} else {
		if p, ok := parsePPS(rbsp[1:]); ok {
			c.ppsMap[ppsIDOf(rbsp[1:])] = p
		}
	}
}

// captureNAL はスライス NAL(type 1/5)のトランスコードを試みる。
// 成功: (ヘッダ RBSP バイト, ヘッダビット数, true, nil)。
// 非スライスや対象外構文: (nil, 0, false, err)。err != nil はストリーム全体を
// 諦めるべき失敗(呼び出し側が素通しへ)。
func (c *h264Capture) captureNAL(nalData []byte) ([]byte, int, bool, error) {
	if len(nalData) < 2 {
		return nil, 0, false, errH264Bits
	}
	hdr := nalData[0]
	typ := int(hdr & 0x1F)
	refIDC := int(hdr >> 5)
	if typ == 7 || typ == 8 {
		c.observePS(nalData)
		return nil, 0, false, nil
	}
	if typ != 1 && typ != 5 {
		return nil, 0, false, nil
	}
	rbsp := unescapeRBSP(nalData)
	payload := rbsp[1:]
	pre := &h264Reader{b: payload}
	if _, err := pre.ue(); err != nil {
		return nil, 0, false, err
	}
	if _, err := pre.ue(); err != nil {
		return nil, 0, false, err
	}
	ppsID, err := pre.ue()
	if err != nil {
		return nil, 0, false, err
	}
	pps := c.ppsMap[int(ppsID)]
	if pps == nil {
		return nil, 0, false, errH264Unsupported
	}
	sps := c.spsMap[pps.spsID]
	if sps == nil || pps.entropyCodingMode {
		return nil, 0, false, errH264Unsupported
	}
	r := &h264Reader{b: payload}
	sl, ok := parseSliceHeader(r, sps, pps, typ, refIDC)
	if !ok {
		return nil, 0, false, errH264Unsupported
	}
	if c.nz == nil || c.nz.mbW != sps.picWidthInMbs || c.nz.mbH != sps.picHeightInMbs {
		c.nz = newH264NZ(sps.picWidthInMbs, sps.picHeightInMbs)
	}
	t := &h264T{capture: true, br: r, enc: c.enc, m: c.model}
	if err := t.transcodeSliceData(sps, sl, c.nz); err != nil {
		return nil, 0, false, err
	}
	hdrBytes := 1 + (sl.headerBits+7)/8
	c.coded++
	return append([]byte(nil), rbsp[:hdrBytes]...), sl.headerBits, true, nil
}

// h264Rebuild は rebuild(算術→ビット)側のエンジン状態。
type h264Rebuild struct {
	spsMap map[int]*h264SPS
	ppsMap map[int]*h264PPS
	dec    *rangeDecoder
	model  *h264Model
	nz     *h264NZ
}

func newH264Rebuild(chunked []byte) *h264Rebuild {
	return &h264Rebuild{
		spsMap: map[int]*h264SPS{},
		ppsMap: map[int]*h264PPS{},
		dec:    newRangeDecoder(chunked),
		model:  newH264Model(),
	}
}

// observePS は capture 側と同じ(スケルトン NAL から取り込む)。
func (d *h264Rebuild) observePS(nalData []byte) {
	(&h264Capture{spsMap: d.spsMap, ppsMap: d.ppsMap}).observePS(nalData)
}

// rebuildNAL はスライス NAL を再構成し EBSP(NALヘッダ込み)を返す。
func (d *h264Rebuild) rebuildNAL(hdrRBSP []byte, hdrBits int) ([]byte, error) {
	if len(hdrRBSP) < 2 {
		return nil, errH264BadRecipe
	}
	nalHdr := hdrRBSP[0]
	typ := int(nalHdr & 0x1F)
	refIDC := int(nalHdr >> 5)
	payload := hdrRBSP[1:]
	pre := &h264Reader{b: payload}
	if _, err := pre.ue(); err != nil {
		return nil, err
	}
	if _, err := pre.ue(); err != nil {
		return nil, err
	}
	ppsID, err := pre.ue()
	if err != nil {
		return nil, err
	}
	pps := d.ppsMap[int(ppsID)]
	if pps == nil {
		return nil, errH264BadRecipe
	}
	sps := d.spsMap[pps.spsID]
	if sps == nil {
		return nil, errH264BadRecipe
	}
	hr := &h264Reader{b: payload}
	sl, ok := parseSliceHeader(hr, sps, pps, typ, refIDC)
	if !ok || sl.headerBits != hdrBits {
		return nil, errH264BadRecipe
	}
	if d.nz == nil || d.nz.mbW != sps.picWidthInMbs || d.nz.mbH != sps.picHeightInMbs {
		d.nz = newH264NZ(sps.picWidthInMbs, sps.picHeightInMbs)
	}
	bw := &h264Writer{}
	cr := &h264Reader{b: hdrRBSP}
	if err := copyBits(bw, cr, 8+hdrBits); err != nil {
		return nil, err
	}
	t := &h264T{capture: false, bw: bw, dec: d.dec, m: d.model}
	if err := t.transcodeSliceData(sps, sl, d.nz); err != nil {
		return nil, err
	}
	bw.u1(1)
	for bw.nbit%8 != 0 {
		bw.u1(0)
	}
	return escapeRBSP(bw.b), nil
}
