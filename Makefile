PROTOC_GEN_GO_VERSION := v1.36.11
PROTOC_GEN_GO_GRPC_VERSION := v1.5.1
PROTO_GO_BIN := $(CURDIR)/.tools/bin

.PHONY: generate generate-go generate-ai clean-generated

generate: generate-go generate-ai

generate-go:
	mkdir -p $(PROTO_GO_BIN)
	GOBIN=$(PROTO_GO_BIN) go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	GOBIN=$(PROTO_GO_BIN) go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)
	PATH=$(PROTO_GO_BIN):$$PATH protoc -I proto -I /usr/include --go_out=. --go_opt=module=github.com/minhphu05/finsights-backend --go-grpc_out=. --go-grpc_opt=module=github.com/minhphu05/finsights-backend $$(find proto -name '*.proto' -print)

generate-ai:
	mkdir -p services/ai/generated
	python3 -m grpc_tools.protoc -I proto -I /usr/include --python_out=services/ai/generated --grpc_python_out=services/ai/generated proto/finsights/v1/ai.proto
	find services/ai/generated -type d -exec touch {}/__init__.py \;

clean-generated:
	rm -rf gen/go services/ai/generated/finsights
