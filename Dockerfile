# ashuku サーバーのコンテナイメージ。
# gzip precompression のためシステム zlib と libzstd(cgo)を使うので、
# ビルドは CGO 有効、実行イメージにも共有ライブラリを含める。

FROM golang:1.25-bookworm AS build
WORKDIR /src
# 依存を先に取得(レイヤキャッシュ)
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# zlib/zstd の開発ヘッダは不要(実行時共有ライブラリに直接リンク)
RUN CGO_ENABLED=1 go build -trimpath -ldflags "-s -w" -o /out/ashuku ./cmd/ashuku && \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/ashuku-cli ./cmd/ashuku-cli

FROM debian:bookworm-slim
# 実行に必要な共有ライブラリ(zlib1g, libzstd1)
RUN apt-get update && \
    apt-get install -y --no-install-recommends zlib1g libzstd1 ca-certificates && \
    rm -rf /var/lib/apt/lists/*
COPY --from=build /out/ashuku /usr/local/bin/ashuku
COPY --from=build /out/ashuku-cli /usr/local/bin/ashuku-cli

# データは名前付きボリュームで永続化する
VOLUME ["/data"]
EXPOSE 8080
# ヘルスチェック(ディスク低下時は 503 を返す)
HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
    CMD ["/bin/sh", "-c", "ashuku-cli -server http://localhost:8080 ls >/dev/null 2>&1 || wget -qO- http://localhost:8080/healthz | grep -q '\"status\":\"ok\"'"]

ENTRYPOINT ["ashuku"]
CMD ["-addr", ":8080", "-data", "/data"]
