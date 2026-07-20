package precomp

// 空白区切りログ(nginx / Apache combined、アクセスログ等)の可逆な列指向変換。
//
// JSONL(§4.31)や CSV(§4.30)の「骨格分離 + 列指向転置」を、区切りが JSON でも
// カンマでもない**空白区切りログ**へ広げる。中核ストレージ用途のログの多くは
// この形で、IsJSONL/IsCSV のどちらにも当たらず素通しだった。
//
// 核心: 各行を「空白で区切ったフィールド」に分けるが、**`"..."` と `[...]` は
// 途中の空白を含めて1フィールドとして原子的に扱う**。これにより nginx/Apache
// combined ログ(引用符付きリクエスト/UA、角括弧付き日時)は毎行フィールド数が
// 一定になり転置できる。骨格(フィールドを 0x00 に潰した行)が全行同一のときだけ
// 対象にし、値を列指向へ転置(列別 raw/delta/dict)。値の中身は一切解釈せず
// 位置だけで往復するので可逆。採用前に復元してバイト一致検証し、読み出し時も
// SHA-256。純Go。

import (
	"bytes"
	"errors"
)

const (
	logMinRows = 8
	logMinCols = 4
	logMaxCols = 4096
)

// LogRecipe は空白区切りログ列指向変換の再構成レシピ。
type LogRecipe struct {
	Skeleton   []byte  `json:"sk"`           // フィールドを 0x00 に置換した行の骨格(全行共通)
	Cols       int     `json:"c"`            // フィールド(列)数
	Rows       int     `json:"r"`            // 行数
	TrailingNL bool    `json:"t,omitempty"`  // 元が改行で終わるか
	Codec      []uint8 `json:"cc,omitempty"` // 列ごとのコーデック(raw/delta/dict)
	ColBytes   []int   `json:"cb,omitempty"` // 列ごとのセグメントバイト長
}

// LogUnwrapped は分解結果。
type LogUnwrapped struct {
	Chunked []byte
	Recipe  *LogRecipe
}

// IsLog は head が空白区切りログらしいかの安価な事前判定。矩形性(全行同一骨格)の
// 本判定は TryUnwrapLog が全体を走査して行う(ここは全体バッファ確保の門番)。
func IsLog(head []byte) bool {
	if len(head) < 16 {
		return false
	}
	line := head
	if i := bytes.IndexByte(head, '\n'); i >= 0 {
		line = head[:i]
	}
	if len(line) < 16 {
		return false
	}
	// JSON/CSV は各専用経路に譲る。
	if line[0] == '{' || line[0] == '[' {
		return false
	}
	spaces := 0
	for _, b := range line {
		if b == 0x00 || b == '\r' {
			return false // 生 0x00 は骨格プレースホルダと衝突、CR は別扱い回避
		}
		if b < 0x20 && b != '\t' {
			return false // 非印字(制御)を含むならログとみなさない
		}
		if b == ' ' {
			spaces++
		}
	}
	// 空白で区切られた複数フィールドがあること(単なる1トークンは対象外)。
	return spaces >= logMinCols-1
}

// logSkeletonize は 1 行を「空白区切り(引用符/角括弧は原子)」で分け、骨格
// (フィールドを 0x00 に潰した行)とフィールド列を返す。骨格+フィールドの
// 連結は構成上つねに元の行に一致する。
func logSkeletonize(line []byte) (skel []byte, fields [][]byte, ok bool) {
	n := len(line)
	if n == 0 {
		return nil, nil, false
	}
	if bytes.IndexByte(line, 0x00) >= 0 {
		return nil, nil, false // 生 0x00 はプレースホルダと衝突
	}
	var sk bytes.Buffer
	sk.Grow(n)
	emit := func(start, end int) {
		fields = append(fields, line[start:end])
		sk.WriteByte(0x00)
	}
	i := 0
	for i < n {
		ch := line[i]
		switch {
		case ch == ' ' || ch == '\t':
			sk.WriteByte(ch)
			i++
		case ch == '"':
			sk.WriteByte('"')
			i++
			start := i
			for i < n && line[i] != '"' {
				if line[i] == '\\' && i+1 < n { // エスケープ(\" 等)を透過
					i++
				}
				i++
			}
			if i >= n {
				return nil, nil, false // 閉じ '"' なし
			}
			emit(start, i)
			sk.WriteByte('"')
			i++
		case ch == '[':
			sk.WriteByte('[')
			i++
			start := i
			for i < n && line[i] != ']' {
				i++
			}
			if i >= n {
				return nil, nil, false // 閉じ ']' なし
			}
			emit(start, i)
			sk.WriteByte(']')
			i++
		default:
			start := i
			for i < n && line[i] != ' ' && line[i] != '\t' && line[i] != '"' && line[i] != '[' {
				i++
			}
			emit(start, i)
		}
	}
	return sk.Bytes(), fields, true
}

// TryUnwrapLog は空白区切りログを列指向に変換する。
func TryUnwrapLog(orig []byte, maxPlain int64) (*LogUnwrapped, bool) {
	if maxPlain <= 0 || maxPlain > maxPlainTotal {
		maxPlain = maxPlainTotal
	}
	if int64(len(orig)) > maxPlain || len(orig) < 64 {
		return nil, false
	}
	lines := bytes.Split(orig, []byte{'\n'})
	trailingNL := false
	if m := len(lines); m > 0 && len(lines[m-1]) == 0 {
		lines = lines[:m-1]
		trailingNL = true
	}
	if len(lines) < logMinRows {
		return nil, false
	}
	skel0, f0, ok := logSkeletonize(lines[0])
	if !ok {
		return nil, false
	}
	ncol := len(f0)
	if ncol < logMinCols || ncol > logMaxCols {
		return nil, false
	}
	nrows := len(lines)
	grid := make([][][]byte, nrows)
	grid[0] = f0
	for r := 1; r < nrows; r++ {
		sk, fs, ok := logSkeletonize(lines[r])
		if !ok || len(fs) != ncol || !bytes.Equal(sk, skel0) {
			return nil, false // 骨格不一致 → 素通し
		}
		grid[r] = fs
	}

	chosen, codecs, colBytes := encodeColumns(grid, ncol, nrows)
	if probeLen(chosen) >= probeLen(orig) {
		return nil, false
	}
	recipe := &LogRecipe{
		Skeleton: append([]byte(nil), skel0...), Cols: ncol, Rows: nrows,
		TrailingNL: trailingNL, Codec: codecs, ColBytes: colBytes,
	}
	if rt, err := ReconstructLog(recipe, chosen); err != nil || !bytes.Equal(rt, orig) {
		return nil, false
	}
	return &LogUnwrapped{Chunked: chosen, Recipe: recipe}, true
}

// ReconstructLog はレシピと列指向ブロブから元のログをバイト単位で戻す。
func ReconstructLog(recipe *LogRecipe, blob []byte) ([]byte, error) {
	if recipe == nil || recipe.Cols < 1 || recipe.Rows < 1 {
		return nil, errors.New("Log レシピが不正です")
	}
	ncol, nrows := recipe.Cols, recipe.Rows
	segs := bytes.Split(recipe.Skeleton, []byte{0x00})
	if len(segs) != ncol+1 {
		return nil, errors.New("Log 骨格のプレースホルダ数が不一致")
	}
	var vals [][][]byte
	if recipe.ColBytes != nil {
		g, err := decodeColumns(blob, recipe.Codec, recipe.ColBytes, ncol, nrows)
		if err != nil {
			return nil, err
		}
		vals = g
	} else {
		return nil, errors.New("Log レシピに列情報がない")
	}
	var out bytes.Buffer
	out.Grow(len(blob) + nrows*len(recipe.Skeleton)/max1(ncol))
	for r := 0; r < nrows; r++ {
		if r > 0 {
			out.WriteByte('\n')
		}
		for c := 0; c < ncol; c++ {
			out.Write(segs[c])
			out.Write(vals[r][c])
		}
		out.Write(segs[ncol])
	}
	if recipe.TrailingNL {
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}
