default: build

build:
	go build ./...

test:
	TF_ACC=0 go test ./...

testacc:
	TF_ACC=1 go test -count=1 -v ./...

mock:
	go run ./mock-control-plane

smoke:
	./scripts/smoke.sh

docs:
	go generate ./...

.PHONY: default build test testacc mock smoke docs
