# steam-ua-proxy（Go 实现）
GO ?= $(shell command -v go 2>/dev/null || echo $(HOME)/apps/go/bin/go)
BIN := steam-ua-proxy

.PHONY: build build-plain test vet fmt clean install

build: ## 静态、去符号表的小体积二进制
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w" -o $(BIN) .

build-plain: ## 不裁剪，便于排查
	CGO_ENABLED=0 $(GO) build -o $(BIN)-plain .

test: build ## 行为对等测试（Go vs Python，需 unshare 可用）
	chmod +x $(CURDIR)/../scripts/run-tests.sh
	$(CURDIR)/../scripts/run-tests.sh $(CURDIR)/$(BIN) $(CURDIR)/../python/steam-ua-proxy.py

vet:
	$(GO) vet ./...

fmt:
	gofmt -w main.go

clean:
	rm -f $(BIN) $(BIN)-plain

install: build ## 装到 ~/.local/bin（覆盖 PyInstaller 版）
	install -m 755 $(BIN) $(HOME)/.local/bin/$(BIN)
	systemctl --user restart steam-ua-proxy.service
