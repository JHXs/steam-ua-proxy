# steam-ua-proxy（Go 实现）
GO ?= $(shell command -v go 2>/dev/null || echo $(HOME)/apps/go/bin/go)
BIN := steam-ua-proxy

# 版本信息在构建时用链接器 -X 注入（源码里的默认值是 "dev"）。
# 打 tag 的提交上 `git describe` 就是 tag 名本身。
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

.PHONY: build build-plain test vet fmt clean install version

build: ## 静态、去符号表的小体积二进制
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) .

build-plain: ## 不裁剪，便于排查
	CGO_ENABLED=0 $(GO) build -o $(BIN)-plain .

version: build ## 打印构建出来的版本
	./$(BIN) -v

test: build ## 行为对等测试（Go vs Python，需 unshare 可用）
	chmod +x $(CURDIR)/../scripts/run-tests.sh
	$(CURDIR)/../scripts/run-tests.sh $(CURDIR)/$(BIN) $(CURDIR)/../reference/steam-ua-proxy.py

vet:
	$(GO) vet ./...

fmt:
	gofmt -w main.go

clean:
	rm -f $(BIN) $(BIN)-plain

install: build ## 装到 ~/.local/bin（覆盖 PyInstaller 版）
	install -m 755 $(BIN) $(HOME)/.local/bin/$(BIN)
	systemctl --user restart steam-ua-proxy.service
