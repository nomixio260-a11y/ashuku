package precomp

// CSV / 区切りテキストの可逆な列指向(columnar)変換。
//
// 行指向の CSV は「1行 = 異種フィールドの並び」なので、圧縮器から見ると
// 型も分布も違う値が交互に来る。列指向に転置すると各列が同種の値
// (タイムスタンプ・列挙・数値)だけになり、LZ/エントロピー符号が桁違いに
// 効く(列ストア=Parquet が小さいのと同じ原理)。実測(疑似アクセスログ
// CSV)で行指向比 **-21.8%**、数値列を差分(delta)符号化するとさらに
// **-25.2%**(RESEARCH.md §4.30)。
//
// 可逆性: 転置は「'\n' で行、',' で列に split → 列順に再連結」で、Go の
// split/join は完全な逆変換。矩形(全行が同じ列数)でありさえすれば、
// 引用符・CRLF の '\r'・非印字バイトも**フィールド内容としてそのまま往復**
// する(RFC4180 の引用解釈はしない=解釈しないから壊れない)。数値列の
// delta は「その列の各値が int64 に正準往復する時だけ」適用する。最後に
// 復元して orig とバイト一致を検証してからのみ採用し、読み出し時も
// SHA-256 で最終検証する。純Go・cgo 不要。

import (
	"bytes"
	"errors"
	"strconv"
)

// csvMinRows / csvMinCols は変換を試す最小規模(小さすぎると利得が出ない)。
const (
	csvMinRows = 8
	csvMinCols = 2
	csvMaxCols = 4096 // 病的に広い行を弾く(メモリ保護)
)

// CSVRecipe は列指向変換の再構成レシピ。
type CSVRecipe struct {
	Cols       int     `json:"c"`            // 列数
	Rows       int     `json:"r"`            // 行数(ヘッダ含む全行)
	TrailingNL bool    `json:"t,omitempty"`  // 元が改行で終わるか
	Delta      []bool  `json:"d,omitempty"`  // 旧: 列ごとの数値 delta フラグ(後方互換)
	Codec      []uint8 `json:"cc,omitempty"` // 新: 列ごとのコーデック(raw/delta/dict)
	ColBytes   []int   `json:"cb,omitempty"` // 新: 列ごとのセグメントバイト長
}

// CSVUnwrapped は分解結果。
type CSVUnwrapped struct {
	Chunked []byte
	Recipe  *CSVRecipe
}

// IsCSV は head が列区切りテキストらしいかの安価な事前判定。矩形性の本判定は
// TryUnwrapCSV が全体を走査して行う(ここは全体バッファ確保の門番)。
func IsCSV(head []byte) bool {
	if len(head) < 4 {
		return false
	}
	// 先頭行(最初の '\n' まで)を見て、印字可能で ',' を含むこと。
	line := head
	if i := bytes.IndexByte(head, '\n'); i >= 0 {
		line = head[:i]
	}
	if len(line) < 3 || bytes.IndexByte(line, ',') < 0 {
		return false
	}
	for _, b := range line {
		// 印字可能 ASCII とタブのみ(先頭行にヌル・制御が混じるなら非CSV)。
		if b != '\t' && (b < 0x20 || b > 0x7E) {
			return false
		}
	}
	return true
}

// parseCanonInt は b が int64 に正準往復する(parse→format が元と一致)
// なら値と true を返す。先頭ゼロ・'+'・"-0" 等は正準でないので false。
func parseCanonInt(b []byte) (int64, bool) {
	if len(b) == 0 || len(b) > 18 {
		return 0, false
	}
	v, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return 0, false
	}
	if strconv.FormatInt(v, 10) != string(b) {
		return 0, false
	}
	return v, true
}

// TryUnwrapCSV は矩形 CSV を列指向(+数値列 delta)に変換する。
func TryUnwrapCSV(orig []byte, maxPlain int64) (*CSVUnwrapped, bool) {
	if maxPlain <= 0 || maxPlain > maxPlainTotal {
		maxPlain = maxPlainTotal
	}
	if int64(len(orig)) > maxPlain || len(orig) < 32 {
		return nil, false
	}
	// 行に分割。末尾改行を検出して除く。
	lines := bytes.Split(orig, []byte{'\n'})
	trailingNL := false
	if n := len(lines); n > 0 && len(lines[n-1]) == 0 {
		lines = lines[:n-1]
		trailingNL = true
	}
	if len(lines) < csvMinRows {
		return nil, false
	}
	ncol := bytes.Count(lines[0], []byte{','}) + 1
	if ncol < csvMinCols || ncol > csvMaxCols {
		return nil, false
	}
	nrows := len(lines)
	// 各行を列に分割し、矩形性を確認。
	grid := make([][][]byte, nrows)
	for i, ln := range lines {
		f := bytes.Split(ln, []byte{','})
		if len(f) != ncol {
			return nil, false // 非矩形 → 変換不可
		}
		grid[i] = f
	}

	// 列ごとに最適コーデック(raw/delta/dict)を選び、連結ブロブを作る。
	chosen, codecs, colBytes := encodeColumns(grid, ncol, nrows)

	// 行指向(原文)より確実に縮む時だけ採用(probe 実測の best-of)。
	if probeLen(chosen) >= probeLen(orig) {
		return nil, false
	}

	recipe := &CSVRecipe{
		Cols: ncol, Rows: nrows, TrailingNL: trailingNL,
		Codec: codecs, ColBytes: colBytes,
	}

	// 最終安全弁: 復元して orig とバイト一致を確認。
	if rt, err := ReconstructCSV(recipe, chosen); err != nil || !bytes.Equal(rt, orig) {
		return nil, false
	}
	return &CSVUnwrapped{Chunked: chosen, Recipe: recipe}, true
}

// probeLen は precomp 共通の probe エンコーダで圧縮したバイト数。
func probeLen(b []byte) int {
	return len(jpegProbeEncoder.EncodeAll(b, make([]byte, 0, len(b)/2)))
}

// ReconstructCSV はレシピと列指向ブロブから元の CSV をバイト単位で戻す。
func ReconstructCSV(recipe *CSVRecipe, blob []byte) ([]byte, error) {
	if recipe == nil || recipe.Cols < 1 || recipe.Rows < 1 {
		return nil, errors.New("CSV レシピが不正です")
	}
	ncol, nrows := recipe.Cols, recipe.Rows
	var fields [][][]byte
	if recipe.ColBytes != nil {
		// 新形式: 列ごとのコーデック+セグメント長で復元。
		g, err := decodeColumns(blob, recipe.Codec, recipe.ColBytes, ncol, nrows)
		if err != nil {
			return nil, err
		}
		fields = g
	} else {
		// 旧形式(後方互換): 各列 nrows 行の固定行グリッド + 一括 delta。
		parts := bytes.Split(blob, []byte{'\n'})
		if len(parts) != ncol*nrows+1 || len(parts[len(parts)-1]) != 0 {
			return nil, errors.New("CSV ブロブの要素数が不一致")
		}
		fields = make([][][]byte, nrows)
		for r := 0; r < nrows; r++ {
			fields[r] = make([][]byte, ncol)
		}
		for c := 0; c < ncol; c++ {
			base := c * nrows
			isDelta := recipe.Delta != nil && c < len(recipe.Delta) && recipe.Delta[c]
			if isDelta {
				row0 := parts[base]
				fields[0][c] = row0
				prev, _ := parseCanonInt(row0) // 非intなら 0
				for r := 1; r < nrows; r++ {
					d, err := strconv.ParseInt(string(parts[base+r]), 10, 64)
					if err != nil {
						return nil, errors.New("CSV delta の解析に失敗")
					}
					v := prev + d
					fields[r][c] = []byte(strconv.FormatInt(v, 10))
					prev = v
				}
			} else {
				for r := 0; r < nrows; r++ {
					fields[r][c] = parts[base+r]
				}
			}
		}
	}
	// 行を ',' で、行同士を '\n' で連結。
	var out bytes.Buffer
	out.Grow(len(blob))
	for r := 0; r < nrows; r++ {
		if r > 0 {
			out.WriteByte('\n')
		}
		for c := 0; c < ncol; c++ {
			if c > 0 {
				out.WriteByte(',')
			}
			out.Write(fields[r][c])
		}
	}
	if recipe.TrailingNL {
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}
