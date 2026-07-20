package precomp

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
	"strings"
	"testing"
	"time"
)

// hevcBitWriter は MSB-first のビット列を組み立てる(h264Reader と同じ並び)。
type hevcBitWriter struct {
	bytes []byte
	nbits int
}

func (w *hevcBitWriter) putBit(b uint) {
	if w.nbits%8 == 0 {
		w.bytes = append(w.bytes, 0)
	}
	if b&1 == 1 {
		w.bytes[len(w.bytes)-1] |= 1 << uint(7-(w.nbits%8))
	}
	w.nbits++
}

func (w *hevcBitWriter) putBits(v uint64, n int) {
	for i := n - 1; i >= 0; i-- {
		w.putBit(uint((v >> uint(i)) & 1))
	}
}

// putUE は符号なし Exp-Golomb を書き込む(h264Reader.ue の逆)。
func (w *hevcBitWriter) putUE(v uint64) {
	x := v + 1
	nb := bits.Len64(x)
	for i := 0; i < nb-1; i++ {
		w.putBit(0)
	}
	w.putBits(x, nb)
}

// buildHEVCSPSPayload は parseHEVCSPS が受け取る RBSP ペイロード(NAL ヘッダ 2
// バイトを除いた本体)を、指定した符号化/変換ブロックサイズと画面寸法で組み立てる。
// max_sub_layers_minus1=0、chroma 4:2:0、SAO/PCM/scaling_list 無効の最小構成。
func buildHEVCSPSPayload(log2MinCb, log2CtbSize, log2MinTb, log2MaxTb, width, height int) []byte {
	w := &hevcBitWriter{}
	w.putBits(0, 4) // sps_video_parameter_set_id
	w.putBits(0, 3) // sps_max_sub_layers_minus1 = 0
	w.putBit(0)     // sps_temporal_id_nesting_flag
	// profile_tier_level: msl=0 なので general PTL の 96 ビットのみ
	w.putBits(0, 32)
	w.putBits(0, 32)
	w.putBits(0, 32)
	w.putUE(0) // sps_seq_parameter_set_id
	w.putUE(1) // chroma_format_idc = 1 (4:2:0)
	w.putUE(uint64(width))
	w.putUE(uint64(height))
	w.putBit(0) // conformance_window_flag
	w.putUE(0)  // bit_depth_luma_minus8
	w.putUE(0)  // bit_depth_chroma_minus8
	w.putUE(0)  // log2_max_pic_order_cnt_lsb_minus4
	w.putBit(0) // sps_sub_layer_ordering_info_present_flag
	w.putUE(0)  // max_dec_pic_buffering_minus1[0]
	w.putUE(0)  // max_num_reorder_pics[0]
	w.putUE(0)  // max_latency_increase_plus1[0]
	w.putUE(uint64(log2MinCb - 3))
	w.putUE(uint64(log2CtbSize - log2MinCb))
	w.putUE(uint64(log2MinTb - 2))
	w.putUE(uint64(log2MaxTb - log2MinTb))
	w.putUE(0)  // max_transform_hierarchy_depth_inter
	w.putUE(0)  // max_transform_hierarchy_depth_intra
	w.putBit(0) // scaling_list_enabled_flag
	w.putBit(0) // amp_enabled_flag
	w.putBit(0) // sample_adaptive_offset_enabled_flag
	w.putBit(0) // pcm_enabled_flag
	w.putUE(0)  // num_short_term_ref_pic_sets
	w.putBit(0) // long_term_ref_pics_present_flag
	w.putBit(0) // sps_temporal_mvp_enabled_flag
	w.putBit(0) // strong_intra_smoothing_enabled_flag
	return w.bytes
}

// TestHEVCSPSBounds は parseHEVCSPS が仕様外の変換/CTB サイズや、min-CB の
// 整数倍でない画面寸法を拒否することを確認する回帰テスト。これらを受理すると
// residual() の固定長走査表 OOB や、空の per-min-block 配列索引 panic を招く。
func TestHEVCSPSBounds(t *testing.T) {
	// ビルダ健全性: 適合 SPS は受理されること(拒否テストの前提)。
	if _, ok := parseHEVCSPS(buildHEVCSPSPayload(3, 6, 2, 5, 64, 64)); !ok {
		t.Fatal("適合 SPS が拒否された(テストビルダ不整合)")
	}
	cases := []struct {
		name    string
		payload []byte
	}{
		// log2MaxTb=6(64x64 変換)→ 8x8 CG 走査表・csbf を溢れさせる。
		{"log2MaxTb>5", buildHEVCSPSPayload(3, 6, 2, 6, 64, 64)},
		// log2CtbSize=7(128x128 CTB)→ csbf グリッド想定超過。
		{"log2CtbSize>6", buildHEVCSPSPayload(3, 7, 2, 5, 64, 64)},
		// width/height が min-CB(64)の整数倍でない → minCbW=width>>6=0。
		{"dimNotMultipleOfMinCb", buildHEVCSPSPayload(6, 6, 2, 5, 16, 16)},
	}
	for _, c := range cases {
		if _, ok := parseHEVCSPS(c.payload); ok {
			t.Errorf("%s: 仕様外 SPS が受理された(境界チェック欠落)", c.name)
		}
	}
}

// TestJPEGSOSWithoutSOF は SOF より前に SOS が来る不正 JPEG で parseFrame が
// 8*hmax によるゼロ除算 panic を起こさず、安全に拒否されることを確認する。
func TestJPEGSOSWithoutSOF(t *testing.T) {
	// FF D8 (SOI), FF DA (SOS) len=6: ns=0,Ss=0,Se=63,AhAl=0、末尾に EOI。
	buf := make([]byte, 128)
	buf[0], buf[1] = 0xFF, 0xD8 // SOI
	buf[2], buf[3] = 0xFF, 0xDA // SOS
	buf[4], buf[5] = 0x00, 0x06 // segment length = 6
	buf[6] = 0x00               // ns = 0(SOF 未検出なので f.comps は空)
	buf[7] = 0x00               // Ss = 0
	buf[8] = 0x3F               // Se = 63
	buf[9] = 0x00               // Ah/Al = 0
	buf[126], buf[127] = 0xFF, 0xD9 // EOI

	// 修正前は parseFrame が f.hmax=0 で 8*hmax 除算し panic する。
	if u, ok := TryUnwrapJPEG(buf, 0); ok || u != nil {
		t.Fatalf("SOF なし JPEG が採用された: ok=%v u=%v", ok, u)
	}
}

// TestMP4H264NegativeHdrLen は ReconstructMP4H264 が負の HdrLen を持つレシピで
// スライス境界 panic(hdrBlob[hp:hp+HdrLen], hp+HdrLen<hp)を起こさず、
// エラーで安全に拒否することを確認する回帰テスト。上限しか見ていなかった旧
// ガードでは負値が素通りしていた(AnnexB 版 ReconstructH264 は switch の
// >0 分岐で弾いており対称化する)。
func TestMP4H264NegativeHdrLen(t *testing.T) {
	recipe := &MP4H264Recipe{
		Segs: []MP4Seg{{RawLen: 0, HdrLen: -1, LenSize: 1}},
		RawN: 0,
		HdrN: 0,
	}
	if _, err := ReconstructMP4H264(recipe, nil); err == nil {
		t.Fatal("負の HdrLen が拒否されなかった")
	}
}

// TestTIFFReadIFDAggregateCap は readIFD が IFD 全体の値確保総数に上限を課し、
// 値領域を重複させて n×個数 に増幅させる OOM 型入力を拒否することを確認する。
// 修正前は per-entry 上限しかなく、20×60000=1.2M 値をすべて確保して受理していた。
func TestTIFFReadIFDAggregateCap(t *testing.T) {
	const n = 20
	const cnt = 60000 // 20*60000 = 1,200,000 > tiffMaxIFDValues(1<<20)
	bo := binary.LittleEndian
	d := make([]byte, 2*cnt) // 値領域(offset 0, SHORT×cnt=120000B)が収まる
	bo.PutUint16(d[8:], n)   // IFD エントリ数
	for i := 0; i < n; i++ {
		e := 10 + i*12
		bo.PutUint16(d[e:], uint16(i)) // 相異なるタグ(map に全保持させる)
		bo.PutUint16(d[e+2:], 3)       // type = SHORT
		bo.PutUint32(d[e+4:], cnt)     // count
		bo.PutUint32(d[e+8:], 0)       // 値オフセット = 0(全エントリで重複)
	}
	if _, ok := readIFD(d, bo, 8); ok {
		t.Fatal("集約確保上限が効かず、増幅 IFD が受理された")
	}
	// 健全性: 上限内の通常 IFD は従来どおり読める。
	d2 := make([]byte, 64)
	bo.PutUint16(d2[8:], 1)
	bo.PutUint16(d2[10:], 256) // tag
	bo.PutUint16(d2[12:], 3)   // SHORT
	bo.PutUint32(d2[14:], 1)   // count = 1
	bo.PutUint16(d2[18:], 42)  // インライン値
	if _, ok := readIFD(d2, bo, 8); !ok {
		t.Fatal("正常な小 IFD が拒否された")
	}
}

// TestPDFFlateScanInflationBudget は PDF の FlateDecode 走査が、失敗候補も含めた
// 累積伸長量に上限を課すことを確認する。修正前は成功時しか予算を課金しなかった
// ため、adler 不一致等で弾かれる伸長爆弾を並べると走査全体で無制限に伸長でき、
// 末尾の有効ストリームまで到達して採用していた(増幅型 CPU/メモリ DoS)。
// 修正後はボム 1 つで予算超過し break、有効ストリームに到達せず未採用となる。
func TestPDFFlateScanInflationBudget(t *testing.T) {
	const maxPlain = 1 << 20 // 1MB
	// ボム: 2MB のゼロを zlib 圧縮(伸長は maxPlain 超 → LimitReader 上限に到達)。
	var bomb bytes.Buffer
	zw := zlib.NewWriter(&bomb)
	zw.Write(make([]byte, 2*maxPlain))
	zw.Close()

	// 末尾の採用可能な小ストリーム(有効 adler・findLevel 一致)。
	content := testText(4096)
	valid, err := ReconstructZlib([]byte{0x78, 0x9c}, 6, content)
	if err != nil {
		t.Fatal(err)
	}

	var pdf bytes.Buffer
	pdf.WriteString("%PDF-1.7\n")
	for k := 0; k < 3; k++ {
		pdf.WriteString("stream\n")
		pdf.Write(bomb.Bytes())
		pdf.WriteString("\nendstream\nendobj\n")
	}
	fmt.Fprintf(&pdf, "1 0 obj\n<</Length %d /Filter /FlateDecode>>\nstream\n", len(valid))
	pdf.Write(valid)
	pdf.WriteString("\nendstream\nendobj\n%%EOF\n")

	if _, _, ok := TryUnwrapPDF(pdf.Bytes(), maxPlain); ok {
		t.Fatal("伸長予算が効かず、ボムの先の有効ストリームが採用された(増幅DoS)")
	}
}

// TestDecodeColumnsRowsBound は decodeColumns が破損 recipe の過大な行数/列数を
// grid 確保前に弾き、recover 不能な OOM を防ぐことを確認する。原文不変量
// (nrows*ncol <= maxPlainTotal)超過は即エラーで、確保は一切行わない。
func TestDecodeColumnsRowsBound(t *testing.T) {
	// nrows を上限超えに設定。個別ガードが make の前に弾くので確保は起きない。
	huge := (1 << 30) + 1
	if _, err := decodeColumns([]byte("x\n"), []uint8{colRaw}, []int{2}, 1, huge); err == nil {
		t.Fatal("過大な nrows が拒否されなかった(OOM 防御の欠落)")
	}
}

// TestDecodeColumnsConstColumnNotRejected は「小さな blob・多い行数」という
// 正当なケース(dictBin の定数列は npal=1 で ID 幅 0 のため行数に依らず数バイト)
// を上限ガードが誤って弾かない=データ損失を起こさないことを確認する回帰テスト。
// 原文サイズ不変量ではなく blob 長で縛ると、この列が誤拒否されて読み出し不能になる。
func TestDecodeColumnsConstColumnNotRejected(t *testing.T) {
	const nrows = 200000
	// 全 nrows 行が "X" の定数列を dictBin 直列化したもの("1\nX\n" の 4 バイト)。
	blob := []byte("1\nX\n")
	grid, err := decodeColumns(blob, []uint8{colDictBin}, []int{len(blob)}, 1, nrows)
	if err != nil {
		t.Fatalf("定数 dictBin 列(小 blob・多行)が誤って拒否された: %v", err)
	}
	if len(grid) != nrows {
		t.Fatalf("行数不一致: got %d want %d", len(grid), nrows)
	}
	for _, r := range []int{0, nrows / 2, nrows - 1} {
		if string(grid[r][0]) != "X" {
			t.Fatalf("row %d: got %q want \"X\"", r, grid[r][0])
		}
	}
}

// TestDeltaBaseCanonFrozen は colDelta の基準導出(凍結境界)の意味論を金値で
// 固定する。基準は encode/decode 双方で使われ、保存物の復元に用いられるため、
// ここが変わると「読めていた物が読めなくなる」データ損失を招く。19桁受理・
// 20桁拒否・非正準拒否を固定し、うっかりした桁規則変更を検知する。
func TestDeltaBaseCanonFrozen(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"1700000000123457789", 1700000000123457789}, // 19桁 ns タイムスタンプ
		{"9223372036854775807", 9223372036854775807}, // int64 最大(19桁)
		{"0", 0},
		{"-123456789012345678", -123456789012345678},
		{"12345678901234567890", 0}, // 20桁 → 非対象(基準 0)
		{"9223372036854775808", 0},  // int64 範囲外 → 0
		{"007", 0},                  // 先頭ゼロ非正準 → 0
		{"+5", 0},                   // '+' 非正準 → 0
		{"", 0},
		{"abc", 0},
	}
	for _, c := range cases {
		if got := deltaBaseCanon([]byte(c.in)); got != c.want {
			t.Errorf("deltaBaseCanon(%q)=%d want %d", c.in, got, c.want)
		}
	}
}

// TestFormatFixedDecMinInt64 は formatFixedDec が math.MinInt64 でも二重マイナスの
// 不正文字列("--...")を生成せず、単一符号の正しい固定小数を返すことを確認する。
func TestFormatFixedDecMinInt64(t *testing.T) {
	got := formatFixedDec(math.MinInt64, 1)
	if strings.HasPrefix(got, "--") {
		t.Fatalf("二重マイナスの不正文字列: %q", got)
	}
	if got != "-922337203685477580.8" {
		t.Fatalf("formatFixedDec(MinInt64,1)=%q want -922337203685477580.8", got)
	}
	// 通常値の挙動が変わっていないこと(回帰防止)。
	if s := formatFixedDec(-5, 2); s != "-0.05" {
		t.Fatalf("formatFixedDec(-5,2)=%q want -0.05", s)
	}
	if s := formatFixedDec(1234, 2); s != "12.34" {
		t.Fatalf("formatFixedDec(1234,2)=%q want 12.34", s)
	}
}

// TestBase64WrappedScanNotQuadratic は base64 折り返し走査が均一幅ランの直後に
// 広い行が来る入力で O(n^2) にならないことを確認する。修正前は各行頭で
// scanB64Wrapped が残り全行を再走査し、K=12000 で数秒〜十数秒を要した。
func TestBase64WrappedScanNotQuadratic(t *testing.T) {
	if testing.Short() {
		t.Skip("長時間入力のためショートモードでは省略")
	}
	line := bytes.Repeat([]byte("A"), 64) // RawStd(48 ゼロバイト)= "A"×64(正準)
	var in bytes.Buffer
	const K = 12000
	for i := 0; i < K; i++ {
		in.Write(line)
		in.WriteByte('\n')
	}
	in.Write(bytes.Repeat([]byte("A"), 68)) // 幅の広い末尾行(均一性を崩す)
	in.WriteByte('\n')

	done := make(chan struct{})
	start := time.Now()
	go func() {
		TryUnwrapBase64(in.Bytes(), 8<<20)
		close(done)
	}()
	select {
	case <-done:
		if el := time.Since(start); el > 3*time.Second {
			t.Fatalf("base64 折り返し走査が遅すぎる(%v)— O(n^2) 回帰の疑い", el)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("base64 折り返し走査が完了しない — O(n^2) 回帰")
	}
}
