# my-market 根 Makefile
#
# proto 代码生成走纯 Go 工具链（本机无 protoc/buf，免 brew 安装）：
#   - 编译器：buf（自带 protobuf 编译器与 well-known types，无需 protoc 二进制）
#   - 插件：protoc-gen-go、protoc-gen-go-grpc
# 三者均 `go install` 到 $(GOBIN)。需要时可把下方的 *_VERSION 固定为具体 tag
# （如 v1.36.10）以获得完全可复现的生成物。
#
# 用法：make proto
# 输入：proto/mall/v1/*.proto（buf.yaml 即在该目录）
# 输出：services/mall-gen/mallv1/*.pb.go（生成物入库，构建不依赖本目标）

GO ?= go
BIN_DIR := $(shell $(GO) env GOPATH)/bin

BUF_VERSION ?= latest
PROTOC_GEN_GO_VERSION ?= latest
PROTOC_GEN_GO_GRPC_VERSION ?= latest

BUF := $(BIN_DIR)/buf
PROTOC_GEN_GO := $(BIN_DIR)/protoc-gen-go
PROTOC_GEN_GO_GRPC := $(BIN_DIR)/protoc-gen-go-grpc

.PHONY: proto proto-tools

# 生成 mall v1 的 Go 代码（消息 + gRPC stub）
proto: $(BUF) $(PROTOC_GEN_GO) $(PROTOC_GEN_GO_GRPC)
	PATH="$(BIN_DIR):$$PATH" $(BUF) generate proto/mall/v1

# 显式安装/升级全部生成工具
proto-tools: $(BUF) $(PROTOC_GEN_GO) $(PROTOC_GEN_GO_GRPC)

$(BUF):
	GOBIN=$(BIN_DIR) $(GO) install github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION)

$(PROTOC_GEN_GO):
	GOBIN=$(BIN_DIR) $(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)

$(PROTOC_GEN_GO_GRPC):
	GOBIN=$(BIN_DIR) $(GO) install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)
