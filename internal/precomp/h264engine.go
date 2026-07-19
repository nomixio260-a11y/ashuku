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
	cmodel *cabacSecModel // CABAC 二次確率(スライス跨ぎ持続、遅延生成)
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
	if sps == nil {
		return nil, 0, false, errH264Unsupported
	}
	if pps.entropyCodingMode {
		return c.captureCABAC(rbsp, payload, sps, pps, typ, refIDC)
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

// captureCABAC は CABAC I スライスを二次算術へ載せ替える(§4.35)。
// 正準 CABAC 再符号化を並走させ、rebuild が生成するバイト列と原文の共通
// 接頭辞長を求める。ヘッダブロブに [ヘッダ][delta 1B][tail] を埋め込み、
// レシピ構造の変更なしで Annex B / MP4 両対応にする。
// 対象は I スライスのみ(P/B は素通し)。
func (c *h264Capture) captureCABAC(rbsp, payload []byte, sps *h264SPS, pps *h264PPS, typ, refIDC int) ([]byte, int, bool, error) {
	r := &h264Reader{b: payload}
	sl, ok := parseSliceHeader(r, sps, pps, typ, refIDC)
	if !ok || (sl.sliceType != 2 && sl.sliceType != 7) {
		return nil, 0, false, errH264Unsupported
	}
	hb := 1 + (sl.headerBits+7)/8 // NALヘッダ+スライスヘッダ+cabac 整列ビット
	if hb >= len(rbsp) {
		return nil, 0, false, errH264Unsupported
	}
	if c.cmodel == nil {
		c.cmodel = newCabacSecModel()
	}
	var stD, stE [1024]uint8
	cabacInitStates(&stD, sl.sliceQP, true, 0)
	cabacInitStates(&stE, sl.sliceQP, true, 0)
	cr := &h264Reader{b: rbsp, pos: hb * 8}
	w := &h264Writer{}
	sink := &cabacTeeSink{d: newCabacDecoder(cr), stD: &stD, rc: c.enc, m: c.cmodel,
		ce: newCabacEncoder(w), stE: &stE}
	mbState := newCabacMBState(sps.picWidthInMbs, sps.picHeightInMbs)
	total := sps.picWidthInMbs * sps.picHeightInMbs
	if !cabacISlice(sink, mbState, sl.firstMB, total, sl.sliceQP) {
		return nil, 0, false, errH264Unsupported
	}
	if rem := len(rbsp)*8 - cr.pos; rem < 0 || rem > 16 {
		return nil, 0, false, errH264Unsupported
	}
	for w.nbit%8 != 0 {
		w.u1(0)
	}
	canon := w.b
	origBody := rbsp[hb:]
	match := 0
	for match < len(canon) && match < len(origBody) && canon[match] == origBody[match] {
		match++
	}
	delta := len(canon) - match
	tail := origBody[match:]
	// tail はエンコーダのフラッシュ差(通常 0〜2 バイト)。大きい場合は
	// 正準側が本体で乖離している=非対応構文の可能性が高いので諦める。
	if delta < 0 || delta > 255 || len(tail) > 64 {
		return nil, 0, false, errH264Unsupported
	}
	hdr := make([]byte, 0, hb+1+len(tail))
	hdr = append(hdr, rbsp[:hb]...)
	hdr = append(hdr, byte(delta))
	hdr = append(hdr, tail...)
	c.coded++
	return hdr, sl.headerBits, true, nil
}

// h264Rebuild は rebuild(算術→ビット)側のエンジン状態。
type h264Rebuild struct {
	spsMap map[int]*h264SPS
	ppsMap map[int]*h264PPS
	dec    *rangeDecoder
	model  *h264Model
	cmodel *cabacSecModel
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
	if pps.entropyCodingMode {
		return d.rebuildCABAC(hdrRBSP, sps, sl)
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

// rebuildCABAC は二次算術から CABAC スライスを正準再符号化し、capture 時に
// 記録した [delta][tail] で原文バイト列へ正確に合わせる。
func (d *h264Rebuild) rebuildCABAC(hdrRBSP []byte, sps *h264SPS, sl *h264Slice) ([]byte, error) {
	hb := 1 + (sl.headerBits+7)/8
	if len(hdrRBSP) < hb+1 {
		return nil, errH264BadRecipe
	}
	delta := int(hdrRBSP[hb])
	tail := hdrRBSP[hb+1:]
	if d.cmodel == nil {
		d.cmodel = newCabacSecModel()
	}
	var st [1024]uint8
	cabacInitStates(&st, sl.sliceQP, true, 0)
	w := &h264Writer{}
	sink := &cabacRebuildSink{dec: d.dec, enc: newCabacEncoder(w), st: &st, m: d.cmodel}
	mbState := newCabacMBState(sps.picWidthInMbs, sps.picHeightInMbs)
	total := sps.picWidthInMbs * sps.picHeightInMbs
	if !cabacISlice(sink, mbState, sl.firstMB, total, sl.sliceQP) {
		return nil, errH264BadRecipe
	}
	for w.nbit%8 != 0 {
		w.u1(0)
	}
	canon := w.b
	match := len(canon) - delta
	if match < 0 {
		return nil, errH264BadRecipe
	}
	rbsp := make([]byte, 0, hb+match+len(tail))
	rbsp = append(rbsp, hdrRBSP[:hb]...)
	rbsp = append(rbsp, canon[:match]...)
	rbsp = append(rbsp, tail...)
	return escapeRBSP(rbsp), nil
}
