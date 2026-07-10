# Go binary to use. Override when `go` is not on PATH in make's shell (e.g. when
# your interactive shell uses an alias): make build GO=/usr/local/go/bin/go
GO            ?= go

pkgs          = $(shell $(GO) list ./...)

PREFIX                  ?= $(shell pwd)
BIN_DIR                 ?= $(shell pwd)
DOCKER_IMAGE_NAME       ?= ncabatoff/process-exporter

BRANCH      ?= $(shell git rev-parse --abbrev-ref HEAD)
BUILDDATE   ?= $(shell date --iso-8601=seconds)
BUILDUSER   ?= $(shell whoami)@$(shell hostname)
REVISION    ?= $(shell git rev-parse HEAD)
TAG_VERSION ?= $(shell git describe --tags --abbrev=0 2>/dev/null || git rev-parse --short HEAD)

VERSION_LDFLAGS := \
  -X github.com/prometheus/common/version.Branch=$(BRANCH) \
  -X github.com/prometheus/common/version.BuildDate=$(BUILDDATE) \
  -X github.com/prometheus/common/version.BuildUser=$(BUILDUSER) \
  -X github.com/prometheus/common/version.Revision=$(REVISION) \
  -X main.version=$(TAG_VERSION)

SMOKE_TEST = -config.path packaging/conf/all.yaml -once-to-stdout-delay 1s |grep -q 'namedprocess_namegroup_memory_bytes{groupname="process-exporte",memtype="virtual"}'

all: format vet test build smoke

style:
	@echo ">> checking code style"
	@! gofmt -d $(shell find . -name '*.go' -print) | grep '^'

test:
	@echo ">> running short tests"
	$(GO) test -short $(pkgs)

format:
	@echo ">> formatting code"
	$(GO) fmt $(pkgs)

vet:
	@echo ">> vetting code"
	$(GO) vet $(pkgs)

build:
	@echo ">> building code"
	cd cmd/process-exporter; CGO_ENABLED=0 $(GO) build -ldflags "$(VERSION_LDFLAGS)" -o ../../process-exporter -a -tags netgo

smoke:
	@echo ">> smoke testing process-exporter"
	./process-exporter $(SMOKE_TEST)

integ:
	@echo ">> integration testing process-exporter"
	$(GO) build -o integration-tester cmd/integration-tester/main.go
	$(GO) build -o load-generator cmd/load-generator/main.go
	./integration-tester -write-size-bytes 65536

install:
	@echo ">> installing binary"
	cd cmd/process-exporter; CGO_ENABLED=0 $(GO) install -a -tags netgo

docker:
	@echo ">> building docker image"
	docker build -t "$(DOCKER_IMAGE_NAME):$(TAG_VERSION)" .
	docker rm configs
	docker create -v /packaging --name configs alpine:3.4 /bin/true
	docker cp packaging/conf configs:/packaging/conf
	docker run --rm --volumes-from configs "$(DOCKER_IMAGE_NAME):$(TAG_VERSION)" $(SMOKE_TEST)

dockertest:
	docker run --rm -it -v `pwd`:/go/src/github.com/ncabatoff/process-exporter golang:1.26  make -C /go/src/github.com/ncabatoff/process-exporter test

dockerinteg:
	docker run --rm -it -v `pwd`:/go/src/github.com/ncabatoff/process-exporter golang:1.26  make -C /go/src/github.com/ncabatoff/process-exporter build integ

.PHONY: update-go-deps
update-go-deps:
	@echo ">> updating Go dependencies"
	@for m in $$($(GO) list -mod=readonly -m -f '{{ if and (not .Indirect) (not .Main)}}{{.Path}}{{end}}' all); do \
		$(GO) get $$m; \
	done
	$(GO) mod tidy

.PHONY: all style format test vet build integ docker
