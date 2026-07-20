package precomp

// base64 で符号化されたバイナリの可逆な「復号→再圧縮」。
//
// base64 はバイナリを 4/3 に膨らませ、6bit の情報を 8bit の 64 記号へ写すので、
// バイト境界がずれて元バイナリの反復構造が崩れ、圧縮器が拾いにくくなる。
// そこで base64 領域を復号してバイナリに戻し(骨格に位置を記録)、圧縮対象を
// 「骨格 + 復号バイナリ列」にする。復元時に**同じ符号器で再符号化**して差し戻す。
//
// 対象: JSON 値・data: URI・JWT パート・生 base64 ダンプ等の**連続 base64**
// (行折り返しの PEM/MIME は将来対応)。標準(+/)と URL-safe(-_)、パディング
// 有無(RawStd/RawURL)を自動判定し、**復号→再符号化が領域と厳密一致する
// 正準 base64 のみ**採用。採用は best-of(zstd 概算)で「確実に縮む時」だけ、
// かつ最終的に全体をバイト一致復元してから。既に圧縮済みのバイナリを base64
// した領域は復号しても縮まないので正しく不採用になる(乱数 base64 等)。

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
)

const (
	// b64MinChars は 1 領域として拾う最小 base64 文字数(短い偶発一致を弾く)。
	b64MinChars = 64
	// b64MinFile は全体をバッファする最小ファイルサイズ。
	b64MinFile = 96
)

// Base64Segment は 1 つの base64 領域のレシピ。
type Base64Segment struct {
	SkelPos int  `json:"p"`           // 骨格内の挿入位置
	RawLen  int  `json:"n"`           // 復号バイナリ長(plains 分割用)
	URL     bool `json:"u,omitempty"` // URL-safe アルファベット(-_)
	Pad     bool `json:"d,omitempty"` // '=' パディング付き
}

// Base64Recipe は base64 分解の再構成レシピ。Chunked = 骨格 + 復号バイナリ列。
type Base64Recipe struct {
	SkelLen  int             `json:"sk"`
	Segments []Base64Segment `json:"sg"`
}

// Base64Unwrapped は分解結果。
type Base64Unwrapped struct {
	Chunked []byte
	Recipe  *Base64Recipe
}

func b64IsAlnum(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

// IsBase64 は head に十分長い base64 連続領域が含まれるか(全体バッファの門番)。
func IsBase64(head []byte) bool {
	if len(head) < b64MinFile {
		return false
	}
	run := 0
	for _, c := range head {
		if b64IsAlnum(c) || c == '+' || c == '/' || c == '-' || c == '_' {
			run++
			if run >= b64MinChars {
				return true
			}
		} else {
			run = 0
		}
	}
	return false
}

// b64Encoding は (url, pad) から符号器を返す。
func b64Encoding(url, pad bool) *base64.Encoding {
	switch {
	case url && pad:
		return base64.URLEncoding
	case url && !pad:
		return base64.RawURLEncoding
	case !url && pad:
		return base64.StdEncoding
	default:
		return base64.RawStdEncoding
	}
}

// scanB64Region は orig[start] から始まる連続 base64 領域を解析する。成功時、
// セグメント・領域バイト長・復号バイナリを返す。非 base64/非正準なら ok=false。
func scanB64Region(orig []byte, start int) (seg Base64Segment, end int, plain []byte, ok bool) {
	n := len(orig)
	// アルファベットを判定しつつ本体(パディング前)を走査。
	hasStd, hasURL := false, false
	j := start
	for j < n {
		c := orig[j]
		if b64IsAlnum(c) {
			j++
			continue
		}
		if c == '+' || c == '/' {
			hasStd = true
			j++
			continue
		}
		if c == '-' || c == '_' {
			hasURL = true
			j++
			continue
		}
		break
	}
	bodyEnd := j
	// パディング '=' を 0..2 個(末尾のみ)。
	pad := 0
	for pad < 2 && j < n && orig[j] == '=' {
		pad++
		j++
	}
	if hasStd && hasURL {
		return seg, 0, nil, false // 混在アルファベットは曖昧 → 対象外
	}
	_ = bodyEnd
	region := orig[start:j]
	if len(region) < b64MinChars {
		return seg, 0, nil, false
	}
	url := hasURL
	padded := pad > 0
	// パディング付きは全体長が 4 の倍数のはず。無しは Raw で任意長を許容。
	if padded && len(region)%4 != 0 {
		return seg, 0, nil, false
	}
	enc := b64Encoding(url, padded)
	dec, err := enc.Strict().DecodeString(string(region))
	if err != nil || len(dec) == 0 {
		return seg, 0, nil, false
	}
	// 正準性: 再符号化が領域と厳密一致すること(復元のバイト一致の要)。
	if enc.EncodeToString(dec) != string(region) {
		return seg, 0, nil, false
	}
	return Base64Segment{RawLen: len(dec), URL: url, Pad: padded}, j, dec, true
}

// TryUnwrapBase64 は base64 領域を復号してチャンク化内容へ差し替える。
func TryUnwrapBase64(orig []byte, maxPlain int64) (*Base64Unwrapped, bool) {
	if maxPlain <= 0 || maxPlain > maxPlainTotal {
		maxPlain = maxPlainTotal
	}
	if int64(len(orig)) > maxPlain || len(orig) < b64MinFile || !IsBase64(orig) {
		return nil, false
	}
	recipe := &Base64Recipe{}
	var skel, plains []byte
	i := 0
	n := len(orig)
	coded := 0
	for i < n {
		c := orig[i]
		if b64IsAlnum(c) || c == '+' || c == '/' || c == '-' || c == '_' {
			seg, end, plain, ok := scanB64Region(orig, i)
			if ok {
				seg.SkelPos = len(skel)
				recipe.Segments = append(recipe.Segments, seg)
				plains = append(plains, plain...)
				coded++
				i = end
				continue
			}
			// 領域として成立しない base64 ラン全体を骨格へ移して飛ばす
			// (O(n^2) 回避のため 1 文字ずつ戻らない)。
			runEnd := i
			for runEnd < n {
				b := orig[runEnd]
				if b64IsAlnum(b) || b == '+' || b == '/' || b == '-' || b == '_' || b == '=' {
					runEnd++
				} else {
					break
				}
			}
			if runEnd == i {
				runEnd = i + 1
			}
			skel = append(skel, orig[i:runEnd]...)
			i = runEnd
			continue
		}
		skel = append(skel, c)
		i++
	}
	if coded == 0 {
		return nil, false
	}
	recipe.SkelLen = len(skel)
	chunked := make([]byte, 0, len(skel)+len(plains))
	chunked = append(chunked, skel...)
	chunked = append(chunked, plains...)

	rj, err := json.Marshal(recipe)
	if err != nil {
		return nil, false
	}
	// 採用ゲート: 保存は zstd 越しなので zstd 概算で比較。
	zo := jpegProbeEncoder.EncodeAll(orig, nil)
	zc := jpegProbeEncoder.EncodeAll(chunked, nil)
	if len(zc)+len(rj)+48 >= len(zo) {
		return nil, false
	}
	rt, err := ReconstructBase64(recipe, chunked)
	if err != nil || !bytes.Equal(rt, orig) {
		return nil, false
	}
	return &Base64Unwrapped{Chunked: chunked, Recipe: recipe}, true
}

// ReconstructBase64 はレシピと Chunked から元のバイト列を戻す。
func ReconstructBase64(recipe *Base64Recipe, chunked []byte) ([]byte, error) {
	if recipe == nil || recipe.SkelLen < 0 || recipe.SkelLen > len(chunked) {
		return nil, errH264BadRecipe
	}
	skel := chunked[:recipe.SkelLen]
	plains := chunked[recipe.SkelLen:]
	var out []byte
	skelPrev, plainOff := 0, 0
	for _, sg := range recipe.Segments {
		if sg.SkelPos < skelPrev || sg.SkelPos > len(skel) ||
			sg.RawLen < 0 || plainOff+sg.RawLen > len(plains) {
			return nil, errH264BadRecipe
		}
		out = append(out, skel[skelPrev:sg.SkelPos]...)
		bin := plains[plainOff : plainOff+sg.RawLen]
		enc := b64Encoding(sg.URL, sg.Pad)
		out = append(out, enc.EncodeToString(bin)...)
		skelPrev = sg.SkelPos
		plainOff += sg.RawLen
	}
	out = append(out, skel[skelPrev:]...)
	return out, nil
}
