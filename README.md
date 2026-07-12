# ashuku(圧縮)— 重複排除+圧縮付きデータ保存サービス

ディスク容量の少ないサーバーでも大量のデータを保存できるようにするための、
**重複排除(デデュープ)+ zstd 圧縮**を内蔵した REST API 型ストレージサービスです。

## 仕組み

アップロードされたデータは次のパイプラインで保存されます:

```
アップロード ──▶ FastCDC チャンク分割(平均1MiB・可変長)
                    │
                    ▼
              SHA-256 で重複判定 ──▶ 既存チャンク → 保存スキップ(重複排除)
                    │
                    ▼ 新規チャンクのみ
              zstd 圧縮(縮まないデータは raw のまま保存)
                    │
                    ▼
              data/chunks/ にコンテンツアドレスで保存
```

- **重複排除**: 同じ内容のチャンクはストア全体で1回しか保存されません。バックアップを
  何世代アップロードしても、変わった部分だけがディスクを消費します。
- **コンテンツ定義チャンキング**: ファイル途中への挿入・変更があっても後続チャンクの
  境界がずれないため、部分的に違うファイル同士でも重複が検出されます。
- **zstd 圧縮**: ユニークチャンクは高圧縮レベルの zstd で圧縮されます。
- **raw フォールバック**: 画像・動画など既に圧縮済みのデータは zstd では縮まないため、
  自動的に無圧縮で保存し、サイズ・CPU の無駄を防ぎます。
- **参照カウント GC**: ファイル削除時、どのファイルからも参照されなくなったチャンク
  だけが物理削除されます。

## 現実的に期待できる削減率

⚠️ 「どんなデータでもテラバイト→数ギガバイト(1000分の1)」は情報理論上不可能です。
削減率はデータの種類に依存します:

| データの種類 | 期待できる削減率 |
|---|---|
| バックアップ/スナップショット(世代間重複が多い) | **10〜100倍以上**(世代数に比例して効果増) |
| ログ・テキスト・JSON/CSV | 5〜20倍以上 |
| 一般ドキュメント(Office・PDF等) | 2〜5倍 |
| 画像・動画・音声(圧縮済み) | ほぼ1倍(raw保存にフォールバック) |

「テラ→数ギガ」が現実に成立するのは、**同じデータを繰り返し保存するバックアップ用途**
(重複排除が支配的)や、**高冗長なログ・テキストデータ**の場合です。
実際の削減効果は `/api/v1/stats` でいつでも確認できます。

## 使い方

### ビルドと起動

```sh
go build -o ashuku ./cmd/ashuku
./ashuku -addr :8080 -data ./data
```

| フラグ | デフォルト | 説明 |
|---|---|---|
| `-addr` | `:8080` | 待ち受けアドレス |
| `-data` | `./data` | データディレクトリ |

### API

| メソッド | パス | 説明 |
|---|---|---|
| `POST` | `/api/v1/files` | アップロード(ボディ=生データ)。名前は `X-File-Name` ヘッダか `?name=` |
| `GET` | `/api/v1/files` | ファイル一覧 |
| `GET` | `/api/v1/files/{id}` | ダウンロード |
| `DELETE` | `/api/v1/files/{id}` | 削除(不要チャンクは自動GC) |
| `GET` | `/api/v1/stats` | 容量統計 |

### 例

```sh
# アップロード
curl -X POST --data-binary @backup.tar -H "X-File-Name: backup.tar" \
     http://localhost:8080/api/v1/files
# → {"id":"3f2a...","name":"backup.tar","size":1073741824,...}

# ダウンロード
curl -o restored.tar http://localhost:8080/api/v1/files/3f2a...

# 一覧
curl http://localhost:8080/api/v1/files

# 削除
curl -X DELETE http://localhost:8080/api/v1/files/3f2a...

# 容量統計
curl http://localhost:8080/api/v1/stats
```

`/api/v1/stats` のレスポンス:

```json
{
  "file_count": 3,
  "chunk_count": 412,
  "logical_bytes": 3221225472,   // アップロードされた見かけの合計サイズ
  "unique_bytes": 429496729,     // 重複排除後のユニークデータ量
  "physical_bytes": 52428800,    // 実際のディスク消費量
  "dedup_ratio": 7.5,            // 重複排除による削減倍率
  "compression_ratio": 8.2,      // 圧縮による削減倍率
  "total_ratio": 61.4,           // 総合削減倍率(logical / physical)
  "saved_bytes": 3168796672      // 節約できた容量
}
```

## 開発

```sh
go test ./...   # テスト実行
go vet ./...    # 静的チェック
```

### 構成

```
cmd/ashuku/          エントリポイント
internal/chunker/    FastCDC チャンカー(github.com/jotfs/fastcdc-go)
internal/store/      ストレージエンジン(dedup / zstd / refcount GC / bbolt メタデータ)
internal/api/        REST API ハンドラ
```

依存はすべて純 Go(CGO 不要)で、シングルバイナリにビルドできます。
