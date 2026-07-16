# ashuku ビルド
#
# release ターゲットは配布用バイナリを作る:
#   -trimpath        : バイナリからビルド環境のファイルパスを除去
#   -ldflags "-s -w" : シンボルテーブル・デバッグ情報を除去
# (リバースエンジニアリングの難度を上げ、内部構造・環境情報の流出を減らす)
#
# サーバーは gzip precompression のため CGO(システムzlib)込みでビルドする。
# クライアントは純Go(CGO不要)なのでクロスコンパイルが容易。

RELEASE_FLAGS := -trimpath -ldflags "-s -w"

.PHONY: build release release-cli test

build:
	go build ./...

release:
	go build $(RELEASE_FLAGS) -o bin/ashuku ./cmd/ashuku
	CGO_ENABLED=0 go build $(RELEASE_FLAGS) -o bin/ashuku-cli ./cmd/ashuku-cli

# 配布用クライアントのクロスコンパイル(ユーザーのデバイス向け)
release-cli:
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build $(RELEASE_FLAGS) -o bin/ashuku-cli-linux-amd64   ./cmd/ashuku-cli
	CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build $(RELEASE_FLAGS) -o bin/ashuku-cli-darwin-arm64  ./cmd/ashuku-cli
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build $(RELEASE_FLAGS) -o bin/ashuku-cli-windows-amd64.exe ./cmd/ashuku-cli

test:
	go vet ./...
	go test ./...
