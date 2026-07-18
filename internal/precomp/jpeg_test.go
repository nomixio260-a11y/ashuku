package precomp

import (
	"bytes"
	"image"
	"image/jpeg"
	"math"
	"math/rand"
	"testing"
)

// makePhoto は写真風の合成画像(グラデーション+テクスチャ+被写体)を
// JPEG(baseline)で符号化して返す。Go の image/jpeg は baseline sequential。
func makePhoto(t *testing.T, w, h, quality int, seed int64) []byte {
	t.Helper()
	img := image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)
	rng := rand.New(rand.NewSource(seed))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			// なめらかなグラデーション + 局所テクスチャ + わずかなノイズ
			base := 128 + 80*math.Sin(float64(x)/40) + 60*math.Cos(float64(y)/55)
			tex := 30 * math.Sin(float64(x*y)/900)
			v := base + tex + float64(rng.Intn(12)-6)
			yi := img.YOffset(x, y)
			img.Y[yi] = clampByte(v)
		}
	}
	for i := range img.Cb {
		img.Cb[i] = clampByte(128 + 40*math.Sin(float64(i)/300))
		img.Cr[i] = clampByte(128 + 35*math.Cos(float64(i)/260))
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func clampByte(v float64) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

// baseline JPEG を分解して元とビット一致で再構成できる。
func TestJPEGRoundTrip(t *testing.T) {
	for _, q := range []int{50, 75, 90, 95} {
		jpg := makePhoto(t, 512, 384, q, 1)
		u, ok := TryUnwrapJPEG(jpg, 0)
		if !ok {
			t.Fatalf("quality=%d: 分解に失敗(または非採用)", q)
		}
		got, err := ReconstructJPEG(u.Recipe, u.Chunked)
		if err != nil {
			t.Fatalf("quality=%d: %v", q, err)
		}
		if !bytes.Equal(got, jpg) {
			t.Fatalf("quality=%d: 再構成がビット一致しません (%d vs %d bytes)", q, len(got), len(jpg))
		}
	}
}

// 分解で保存見込みが縮む(採用される)ことと、圧縮の効きを確認する。
func TestJPEGCompressionGain(t *testing.T) {
	jpg := makePhoto(t, 1024, 768, 85, 7)
	u, ok := TryUnwrapJPEG(jpg, 0)
	if !ok {
		t.Skip("この画像では分解が採用されなかった(悪化回避)")
	}
	probe := jpegProbeEncoder.EncodeAll(u.Chunked, nil)
	gain := 100 * (1 - float64(len(probe))/float64(len(jpg)))
	t.Logf("JPEG %d bytes → 分解+zstd 見込み %d bytes (%.1f%% 削減)", len(jpg), len(probe), gain)
	if len(probe) >= len(jpg) {
		t.Fatalf("採用されたのに縮んでいません: %d >= %d", len(probe), len(jpg))
	}
}

// 似た画像(同一被写体の微小な差)は係数平面が似るため、チャンク重複排除・
// デルタ圧縮の対象になる。ここでは「2枚の分解結果の共通部分が大きい」ことを
// 簡易に確認する(実際の dedup は store 側テストで検証)。
func TestJPEGSimilarImagesShareData(t *testing.T) {
	a := makePhoto(t, 512, 384, 85, 1)
	b := makePhoto(t, 512, 384, 85, 1) // 同一シード = 同一画像
	ua, oka := TryUnwrapJPEG(a, 0)
	ub, okb := TryUnwrapJPEG(b, 0)
	if !oka || !okb {
		t.Skip("分解が採用されなかった")
	}
	// 同一画像なら分解結果も完全一致(=チャンクが100%共有される)
	if !bytes.Equal(ua.Chunked, ub.Chunked) {
		t.Fatal("同一画像の分解結果が一致しません")
	}
}

// プログレッシブ JPEG は対象外(素通し)。Go はオプションで出せないので、
// SOF2 マーカを含む最小の細工データで拒否を確認する。
func TestJPEGRejectsNonBaseline(t *testing.T) {
	// baseline を作り、SOF0(FFC0)を SOF2(FFC2)に書き換えると parseFrame が拒否
	jpg := makePhoto(t, 128, 128, 80, 1)
	mangled := append([]byte(nil), jpg...)
	for i := 0; i+1 < len(mangled); i++ {
		if mangled[i] == 0xFF && mangled[i+1] == 0xC0 {
			mangled[i+1] = 0xC2 // SOF2(プログレッシブ)
			break
		}
	}
	if _, ok := TryUnwrapJPEG(mangled, 0); ok {
		t.Fatal("非 baseline を受理してしまいました")
	}
}
