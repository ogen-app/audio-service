# Shared gRPC contract (CON-220). audio.v1 lives in buf.build/ogen-app/proto;
# this repo generates the server stubs from a pinned version of that module and
# commits gen/ so `go build` needs no buf. Bump PROTO_VERSION to adopt a new
# contract, then `make proto` and commit gen/.
#
# NOTE: the audio.v1 addition (CON-282) is not yet published to BSR. Until it
# is, generate locally from a checkout of the proto repo:
#
#   buf generate ../proto/proto --path proto/audio/v1
#
# Once audio.v1 ships in a tagged buf.build/ogen-app/proto release, bump
# PROTO_VERSION below (>= v1.2.0) and switch `make proto` back to the BSR module,
# then `make proto` and commit gen/.
PROTO_MODULE  = buf.build/ogen-app/proto
PROTO_VERSION = v1.2.0

.PHONY: proto build test

proto:
	buf generate $(PROTO_MODULE):$(PROTO_VERSION) --path audio/v1/audio.proto

build:
	CGO_ENABLED=0 go build ./...

test:
	go test -race ./...
