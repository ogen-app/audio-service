# Shared gRPC contract (CON-220). audio.v1 lives in buf.build/ogen-app/proto
# (published in v1.2.0, CON-282); this repo generates the server stubs from a
# pinned version of that module and commits gen/ so `go build` needs no buf.
# Bump PROTO_VERSION to adopt a new contract, then `make proto` and commit gen/.
PROTO_MODULE  = buf.build/ogen-app/proto
PROTO_VERSION = v1.2.0

.PHONY: proto build test

proto:
	buf generate $(PROTO_MODULE):$(PROTO_VERSION) --path audio/v1/audio.proto

build:
	CGO_ENABLED=0 go build ./...

test:
	go test -race ./...
