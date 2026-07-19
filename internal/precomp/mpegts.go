package precomp

// MPEG-TS(トランスポートストリーム)内 H.264 の可逆再圧縮。
//
// ドラレコ・DVR・テレビ録画・HLS セグメントの定番コンテナ。188 バイト固定
// パケットの列で、映像は PES パケットに包まれた H.264 Annex B ストリーム。
//
// 設計: TS パケットヘッダ・適応フィールド・PES ヘッダ・他 PID(音声・PSI)は
// **全て原文のまま骨格に保持**し、映像 PID の ES バイト列だけを連結して
// 既存の H.264 エンジン(TryUnwrapH264 と同じ capture/rebuild)に委譲する。
// 再構成は正準 ES を骨格の記録位置へ差し戻すだけ。パケット構造・連続性
// カウンタ・PCR には一切触れないため、ES がバイト一致すればファイル全体が
// バイト一致する。
//
// 対象外(安全に素通し): スクランブル、複数映像 PID、H.264 以外の映像。

import (
	"bytes"
	"encoding/binary"
)

const tsPacketSize = 188

// IsTS は先頭が MPEG-TS らしいか(0x47 同期が2連続)を判定する。
func IsTS(head []byte) bool {
	if len(head) < tsPacketSize+1 {
		return len(head) > 0 && head[0] == 0x47
	}
	return head[0] == 0x47 && head[tsPacketSize] == 0x47
}

// TSRun は「骨格 Raw バイト+ES バイト」の対の N 回繰り返し。
// TS はほぼ同形パケット(4B ヘッダ+184B ES)の連続なので RLE が強く効く。
type TSRun struct {
	Raw int `json:"r,omitempty"`
	ES  int `json:"e,omitempty"`
	N   int `json:"n,omitempty"` // 省略時 1
}

// TSRecipe は TS 再構成レシピ。Chunked = 骨格ブロブ + H264 の Chunked。
type TSRecipe struct {
	Runs  []TSRun     `json:"runs"`
	SkelN int         `json:"sn"`
	H264  *H264Recipe `json:"h264"`
}

// TSUnwrapped は分解結果。
type TSUnwrapped struct {
	Chunked []byte
	Recipe  *TSRecipe
}

// tsParse は TS を走査し、映像 ES のバイト範囲(runs)を求める。
// 戻り値: runs(Raw/ES 交互)、連結 ES、ok。
func tsParse(orig []byte) ([]TSRun, []byte, bool) {
	n := len(orig)
	if n < tsPacketSize || n%tsPacketSize != 0 {
		return nil, nil, false
	}
	// --- PAT/PMT で H.264 の PID を見つける ---
	videoPID := -1
	pmtPID := -1
	for off := 0; off+tsPacketSize <= n && videoPID < 0; off += tsPacketSize {
		p := orig[off : off+tsPacketSize]
		if p[0] != 0x47 {
			return nil, nil, false
		}
		pusi := p[1]&0x40 != 0
		pid := int(p[1]&0x1F)<<8 | int(p[2])
		afc := (p[3] >> 4) & 3
		if afc&1 == 0 || !pusi {
			continue
		}
		payload := p[4:]
		if afc&2 != 0 { // 適応フィールドを飛ばす
			al := int(payload[0])
			if 1+al >= len(payload) {
				continue
			}
			payload = payload[1+al:]
		}
		// PSI: pointer_field
		if len(payload) < 1 {
			continue
		}
		pf := int(payload[0])
		if 1+pf >= len(payload) {
			continue
		}
		sec := payload[1+pf:]
		if len(sec) < 8 {
			continue
		}
		tableID := sec[0]
		secLen := int(sec[1]&0x0F)<<8 | int(sec[2])
		if 3+secLen > len(sec) {
			continue
		}
		if pid == 0 && tableID == 0 { // PAT
			// program loop: [program_number(16) reserved(3) PID(13)] × N
			for i := 8; i+4 <= 3+secLen-4; i += 4 {
				prog := int(sec[i])<<8 | int(sec[i+1])
				if prog != 0 {
					pmtPID = int(sec[i+2]&0x1F)<<8 | int(sec[i+3])
					break
				}
			}
		} else if pid == pmtPID && tableID == 2 { // PMT
			if len(sec) < 12 {
				continue
			}
			piLen := int(sec[10]&0x0F)<<8 | int(sec[11])
			i := 12 + piLen
			for i+5 <= 3+secLen-4 {
				st := sec[i]
				esPID := int(sec[i+1]&0x1F)<<8 | int(sec[i+2])
				esLen := int(sec[i+3]&0x0F)<<8 | int(sec[i+4])
				if st == 0x1B { // H.264
					if videoPID >= 0 && videoPID != esPID {
						return nil, nil, false // 複数映像 PID は対象外
					}
					videoPID = esPID
				}
				i += 5 + esLen
			}
		}
	}
	if videoPID < 0 {
		return nil, nil, false
	}
	// --- 映像 PID の ES バイト範囲を求める ---
	var runs []TSRun
	var es []byte
	rawStart := 0
	addES := func(off, l int) {
		if l <= 0 {
			return
		}
		raw := off - rawStart
		if k := len(runs); k > 0 && runs[k-1].Raw == raw && runs[k-1].ES == l {
			runs[k-1].N++
		} else {
			runs = append(runs, TSRun{Raw: raw, ES: l, N: 1})
		}
		es = append(es, orig[off:off+l]...)
		rawStart = off + l
	}
	for off := 0; off+tsPacketSize <= n; off += tsPacketSize {
		p := orig[off : off+tsPacketSize]
		pid := int(p[1]&0x1F)<<8 | int(p[2])
		if pid != videoPID {
			continue
		}
		if p[1]&0x80 != 0 { // transport_error
			return nil, nil, false
		}
		if p[3]&0xC0 != 0 { // スクランブル
			return nil, nil, false
		}
		pusi := p[1]&0x40 != 0
		afc := (p[3] >> 4) & 3
		if afc&1 == 0 {
			continue // 適応のみ
		}
		body := 4
		if afc&2 != 0 {
			al := int(p[4])
			body = 5 + al
			if body > tsPacketSize {
				return nil, nil, false
			}
		}
		payOff := off + body
		payLen := tsPacketSize - body
		if payLen <= 0 {
			continue
		}
		if pusi {
			// PES ヘッダ: 00 00 01 E0-EF len(2) flags(2) hdrLen(1) [hdr...]
			pay := orig[payOff : payOff+payLen]
			if payLen < 9 || pay[0] != 0 || pay[1] != 0 || pay[2] != 1 || pay[3]&0xF0 != 0xE0 {
				return nil, nil, false
			}
			pesLen := int(binary.BigEndian.Uint16(pay[4:]))
			if pesLen != 0 {
				// 有限長 PES(映像では稀)。長さ管理が複雑になるため対象外。
				return nil, nil, false
			}
			hdrLen := int(pay[8])
			esStart := 9 + hdrLen
			if esStart > payLen {
				return nil, nil, false
			}
			addES(payOff+esStart, payLen-esStart)
		} else {
			addES(payOff, payLen)
		}
	}
	if len(es) == 0 {
		return nil, nil, false
	}
	// 末尾の骨格
	if rawStart < n {
		runs = append(runs, TSRun{Raw: n - rawStart, N: 1})
	}
	return runs, es, true
}

// TryUnwrapTS は TS 内の H.264 を再符号化する。
func TryUnwrapTS(orig []byte, maxPlain int64) (*TSUnwrapped, bool) {
	if maxPlain <= 0 || maxPlain > maxPlainTotal {
		maxPlain = maxPlainTotal
	}
	if int64(len(orig)) > maxPlain || len(orig) < tsPacketSize*4 || !IsTS(orig) {
		return nil, false
	}
	runs, es, ok := tsParse(orig)
	if !ok || !IsH264(es) {
		return nil, false
	}
	u, ok := TryUnwrapH264(es, maxPlain)
	if !ok {
		return nil, false
	}
	// 骨格 = ES を除いた全バイト
	skel := make([]byte, 0, len(orig)-len(es))
	pos := 0
	for _, r := range runs {
		for k := 0; k < r.N; k++ {
			skel = append(skel, orig[pos:pos+r.Raw]...)
			pos += r.Raw + r.ES
		}
	}
	recipe := &TSRecipe{Runs: runs, SkelN: len(skel), H264: u.Recipe}
	chunked := make([]byte, 0, len(skel)+len(u.Chunked))
	chunked = append(chunked, skel...)
	chunked = append(chunked, u.Chunked...)
	if len(chunked)+len(runs)*16+96 >= len(orig) {
		return nil, false
	}
	rt, err := ReconstructTS(recipe, chunked)
	if err != nil || !bytes.Equal(rt, orig) {
		return nil, false
	}
	return &TSUnwrapped{Chunked: chunked, Recipe: recipe}, true
}

// ReconstructTS はレシピと Chunked から元の TS をバイト単位で戻す。
func ReconstructTS(recipe *TSRecipe, chunked []byte) ([]byte, error) {
	if recipe == nil || recipe.H264 == nil || recipe.SkelN < 0 || recipe.SkelN > len(chunked) {
		return nil, errH264BadRecipe
	}
	skel := chunked[:recipe.SkelN]
	es, err := ReconstructH264(recipe.H264, chunked[recipe.SkelN:])
	if err != nil {
		return nil, err
	}
	var out []byte
	sp, ep := 0, 0
	for _, r := range recipe.Runs {
		nrep := r.N
		if nrep == 0 {
			nrep = 1
		}
		for k := 0; k < nrep; k++ {
			if sp+r.Raw > len(skel) || ep+r.ES > len(es) {
				return nil, errH264BadRecipe
			}
			out = append(out, skel[sp:sp+r.Raw]...)
			sp += r.Raw
			out = append(out, es[ep:ep+r.ES]...)
			ep += r.ES
		}
	}
	if ep != len(es) {
		return nil, errH264BadRecipe
	}
	return out, nil
}
