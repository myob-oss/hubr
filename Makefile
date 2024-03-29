# Borrowed from:
# https://github.com/silven/go-example/blob/master/Makefile

BINARY = hubr
VET_REPORT = vet.report
GOARCH = amd64

VERSION?=0.6.7
COMMIT=$(shell git rev-parse HEAD)
BRANCH=$(shell git rev-parse --abbrev-ref HEAD)

BUILD_DIR=${GOPATH}/hubr
CURRENT_DIR=$(shell pwd)
BUILD_DIR_LINK=$(shell readlink ${BUILD_DIR})

# Setup the -ldflags option for go build here, interpolate the variable values
LDFLAGS = -ldflags "-X main.hubr=${VERSION} -X main.COMMIT=${COMMIT} -X main.BRANCH=${BRANCH}"

# Build the project
all: clean install-deps test vet linux darwin windows

linux:
	cd ${BUILD_DIR}; \
	GOOS=linux GOARCH=${GOARCH} go build ${LDFLAGS} -o bin/${BINARY}-linux-${GOARCH} . ; \
	cd - >/dev/null

darwin:
	cd ${BUILD_DIR}; \
	GOOS=darwin GOARCH=${GOARCH} go build ${LDFLAGS} -o bin/${BINARY}-darwin-${GOARCH} . ; \
	cd - >/dev/null

windows:
	cd ${BUILD_DIR}; \
	GOOS=windows GOARCH=${GOARCH} go build ${LDFLAGS} -o bin/${BINARY}-windows-${GOARCH}.exe . ; \
	cd - >/dev/null

zip-code:
	# TODO zip the code

update:
	go get -u ./...

install-deps:
	go mod download
	go install

test:
	go clean -testcache
	go test ./... -v

vet:
	go list ./...
	go vet ./... > ${VET_REPORT} 2>&1 ; \

fmt:
	cd ${BUILD_DIR}; \
	go fmt $$(go list ./... | grep -v /vendor/) ; \
	cd - >/dev/null

clean:
	-rm -f ${TEST_REPORT}
	-rm -f ${VET_REPORT}
	-rm -rf bin

.PHONY: all linux darwin windows update install-deps test vet fmt clean zip-code
