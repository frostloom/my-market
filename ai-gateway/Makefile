# ai-gateway Makefile（仅 proto 代码生成，模式对齐 my-market 仓根 Makefile）
#
# 纯 Go 工具链（本机无 protoc 二进制）：
#   - 编译器：buf（自带 protobuf 编译器与 well-known types）
#   - 插件：protoc-gen-go、protoc-gen-go-grpc
# 生成配置见 buf.gen.yaml；输入 pkg/proto/billing/v1/billing.proto，
# 输出 pkg/proto/billing/v1/{billing.pb.go,billing_grpc.pb.go}（生成物入库，构建不依赖本目标）。
# 生成物须与仓内版本可复现对齐时可把 *_VERSION 固定为具体 tag（当前实际：buf 1.75.0 /
# protoc-gen-go v1.36.12 / protoc-gen-go-grpc v1.6.2）。
#
# 用法：make proto

GO ?= go
BIN_DIR := $(shell $(GO) env GOPATH)/bin

BUF_VERSION ?= latest
PROTOC_GEN_GO_VERSION ?= latest
PROTOC_GEN_GO_GRPC_VERSION ?= latest

BUF := $(BIN_DIR)/buf
PROTOC_GEN_GO := $(BIN_DIR)/protoc-gen-go
PROTOC_GEN_GO_GRPC := $(BIN_DIR)/protoc-gen-go-grpc

.PHONY: proto proto-tools

# 生成 billing v1 的 Go 代码（消息 + gRPC stub）
proto: $(BUF) $(PROTOC_GEN_GO) $(PROTOC_GEN_GO_GRPC)
	PATH="$(BIN_DIR):$$PATH" $(BUF) generate pkg/proto

# 显式安装/升级全部生成工具
proto-tools: $(BUF) $(PROTOC_GEN_GO) $(PROTOC_GEN_GO_GRPC)

$(BUF):
	GOBIN=$(BIN_DIR) $(GO) install github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION)

$(PROTOC_GEN_GO):
	GOBIN=$(BIN_DIR) $(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)

$(PROTOC_GEN_GO_GRPC):
	GOBIN=$(BIN_DIR) $(GO) install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)
