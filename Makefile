# AgentCarto 本体とプラグイン（サブプロセス実行ファイル）の開発・動作確認用ショートカット。
# プラグインと SDK は別リポジトリ（兄弟ディレクトリ）に置かれている前提（README「プラグイン構成」）。
# 親ディレクトリに go.work があれば全モジュールを統合してビルドする。
#
# 本体は各プラグインを「サブプロセス（独立実行ファイル）」として起動するため、本体バイナリに加えて
# bin/agentcarto-plugin-<type> を生成する必要がある（make build がまとめて生成）。

BIN      := bin/agentcarto
BINDIR   := bin
CONFIG   := $(BINDIR)/config.yaml
SIBLINGS := ../agentcarto-core ../plugin-claude ../plugin-codex ../plugin-grok ../plugin-copilot

.PHONY: build plugins config run test check validate plugins-list doctor list active clean

## build: 本体バイナリ・全プラグイン実行ファイル・動作確認用 config を bin/ に揃える
build: plugins config
	go build -o $(BIN) ./cmd/agentcarto

## config: 動作確認用の設定ファイル bin/config.yaml を用意（既存なら維持）
config:
	@mkdir -p $(BINDIR)
	@[ -f $(CONFIG) ] || cp config.example.yaml $(CONFIG)
	@echo "config ready: $(CONFIG)"

## plugins: 各プラグインの実行ファイルを bin/ に生成（本体が子プロセスとして起動する）
plugins:
	go build -o $(BINDIR)/agentcarto-plugin-claude     ../plugin-claude/cmd/agentcarto-plugin-claude
	go build -o $(BINDIR)/agentcarto-plugin-codex      ../plugin-codex/cmd/agentcarto-plugin-codex
	go build -o $(BINDIR)/agentcarto-plugin-grok       ../plugin-grok/cmd/agentcarto-plugin-grok
	go build -o $(BINDIR)/agentcarto-plugin-copilot-vc ../plugin-copilot/cmd/agentcarto-plugin-copilot-vc
	go build -o $(BINDIR)/agentcarto-plugin-copilot-jb ../plugin-copilot/cmd/agentcarto-plugin-copilot-jb

## run: ビルドして TUI を起動
run: build
	./$(BIN)

## test: 本体のテスト（プラグインバイナリを使う統合テストを含むため先に plugins を生成）
test: plugins
	go test ./...

## check: 本体＋全プラグイン/SDK リポジトリを横断して build + test
check: plugins
	@echo "== agentcarto (本体) =="
	@go build ./... && go test ./...
	@for d in $(SIBLINGS); do \
		echo "== $$d =="; \
		( cd $$d && go build ./... && go test ./... ) || exit 1; \
	done
	@echo "ALL OK"

## validate: 設定を検証（bin/config.yaml は --config なしで自動読み込み）
validate: build
	./$(BIN) config validate

## plugins-list: プラグインと capability を一覧
plugins-list: build
	./$(BIN) plugins list

## doctor: 設定・実行ファイル・保存場所を診断
doctor: build
	./$(BIN) doctor

## list: 保存済みセッションを一覧（読み取り専用）
list: build
	./$(BIN) list

## active: 稼働中セッションを一覧
active: build
	./$(BIN) active

## clean: 生成物を削除
clean:
	rm -f $(BINDIR)/agentcarto $(BINDIR)/agentcarto-plugin-*
