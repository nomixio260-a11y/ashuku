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
	// 行折り返し(PEM/MIME)。LineW>0 で復元時に幅 LineW ごとに Sep で折る。
	LineW int  `json:"w,omitempty"` // 0=連続。>0=折り返し幅
	CRLF  bool `json:"r,omitempty"` // 区切りが \r\n(既定 \n)
	Trail bool `json:"t,omitempty"` // 最終行の後にも区切りが付く
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

// b64Rewrap は base64 文字列 chars を幅 w ごとに sep で折る(PEM/MIME 復元)。
func b64Rewrap(chars string, w int, crlf, trail bool) []byte {
	sep := "\n"
	if crlf {
		sep = "\r\n"
	}
	var out []byte
	for i := 0; i < len(chars); i += w {
		e := i + w
		if e > len(chars) {
			e = len(chars)
		}
		out = append(out, chars[i:e]...)
		if e < len(chars) || trail {
			out = append(out, sep...)
		}
	}
	return out
}

// isB64Data は c が base64 データ文字か(パディングと区切りは除く)を返し、
// アルファベット種別を更新する。
func isB64Data(c byte, hasStd, hasURL *bool) bool {
	if b64IsAlnum(c) {
		return true
	}
	if c == '+' || c == '/' {
		*hasStd = true
		return true
	}
	if c == '-' || c == '_' {
		*hasURL = true
		return true
	}
	return false
}

// scanB64Wrapped は orig[start] から始まる**行折り返し**(均一幅+一定区切り)の
// base64 ブロックを 1 セグメントとして解析する。PEM/MIME(証明書束・大きな添付)
// を多数の行別セグメントに割らず、レシピを小さく保つ。均一でない/1 行のみは
// ok=false(連続スキャナへ委ねる)。復元 rewrap が領域と厳密一致する時のみ採用。
func scanB64Wrapped(orig []byte, start int) (seg Base64Segment, end int, plain []byte, ok bool) {
	n := len(orig)
	var hasStd, hasURL bool
	// 1 行目
	i := start
	for i < n && isB64Data(orig[i], &hasStd, &hasURL) {
		i++
	}
	w := i - start
	if w < 24 { // 折り返しとみなすには狭すぎ(連続スキャナに任せる)
		return seg, 0, nil, false
	}
	var crlf bool
	if i < n && orig[i] == '\n' {
		crlf = false
	} else if i+1 < n && orig[i] == '\r' && orig[i+1] == '\n' {
		crlf = true
	} else {
		return seg, 0, nil, false // 区切りなし=折り返しでない
	}
	sepLen := 1
	if crlf {
		sepLen = 2
	}
	var b64 []byte
	b64 = append(b64, orig[start:i]...)
	j := i
	lines := 1
	trail := false
	for {
		// 区切りを消費
		j += sepLen
		ls := j
		for j < n && isB64Data(orig[j], &hasStd, &hasURL) {
			j++
		}
		lineW := j - ls
		pad := 0
		for pad < 2 && j < n && orig[j] == '=' {
			pad++
			j++
		}
		total := lineW + pad
		if total == 0 { // 区切りの直後がすぐ非 base64 → 直前の区切りは末尾
			trail = true
			break
		}
		if total > w {
			// 幅が広がる=均一でない。前方走査位置 ls を返して呼び出し側が
			// この範囲での折り返し再試行を抑止できるようにする(O(n^2) 回避)。
			return seg, ls, nil, false
		}
		b64 = append(b64, orig[ls:j]...)
		lines++
		// 次に同種区切り+続きがあるか
		var nextSep bool
		if !crlf && j < n && orig[j] == '\n' {
			nextSep = true
		} else if crlf && j+1 < n && orig[j] == '\r' && orig[j+1] == '\n' {
			nextSep = true
		}
		if total == w && pad == 0 && nextSep {
			continue // 満行 → 続行
		}
		// 最終行(短い/パディング/次に区切りなし)
		if nextSep {
			trail = true
			j += sepLen
		}
		break
	}
	if lines < 2 || hasStd && hasURL {
		return seg, 0, nil, false // 1 行のみ or 混在アルファベット
	}
	region := orig[start:j]
	url := hasURL
	padded := len(b64) > 0 && b64[len(b64)-1] == '='
	enc := b64Encoding(url, padded)
	dec, err := enc.Strict().DecodeString(string(b64))
	if err != nil || len(dec) == 0 {
		return seg, 0, nil, false
	}
	// 正準性 + 折り返し再現が領域と厳密一致すること。
	if !bytes.Equal(b64Rewrap(enc.EncodeToString(dec), w, crlf, trail), region) {
		return seg, 0, nil, false
	}
	return Base64Segment{RawLen: len(dec), URL: url, Pad: padded, LineW: w, CRLF: crlf, Trail: trail}, j, dec, true
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
	// noWrapUntil: 均一幅ラン直後に広い行が来て scanB64Wrapped が失敗した前方位置。
	// その範囲では折り返しは決して成立しない(どの開始点も同じ広い行で失敗する)ため、
	// ここまで折り返し試行を抑止して 1 行ずつ再走査する O(n^2) を防ぐ。
	noWrapUntil := 0
	for i < n {
		c := orig[i]
		if b64IsAlnum(c) || c == '+' || c == '/' || c == '-' || c == '_' {
			// 折り返しブロックを優先(PEM/MIME を 1 セグメントに畳む)、
			// だめなら連続 1 行として解析。
			if i >= noWrapUntil {
				if seg, end, plain, ok := scanB64Wrapped(orig, i); ok {
					seg.SkelPos = len(skel)
					recipe.Segments = append(recipe.Segments, seg)
					plains = append(plains, plain...)
					coded++
					i = end
					continue
				} else if end > noWrapUntil {
					// total>w で前方まで走査して失敗 → その範囲は折り返し不成立。
					noWrapUntil = end
				}
			}
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
		chars := enc.EncodeToString(bin)
		if sg.LineW > 0 { // 折り返し(PEM/MIME)を再現
			out = append(out, b64Rewrap(chars, sg.LineW, sg.CRLF, sg.Trail)...)
		} else {
			out = append(out, chars...)
		}
		skelPrev = sg.SkelPos
		plainOff += sg.RawLen
	}
	out = append(out, skel[skelPrev:]...)
	return out, nil
}
