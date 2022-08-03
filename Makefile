# GOFILES = $(shell find . -name '*.go' -not -path './vendor/*')

default: test

clean:
	go clean ./...

build:
	go build ./...

test:
	go test ./... -v --coverprofile=coverage.txt --covermode=atomic

update-deps:
	go get -u ./... & go mod tidy

update-changelog:
	conventional-changelog -p angular -i CHANGELOG.md -s

list-deps:
	go list -f '{{.Deps}}' ./... | tr "[" " " | tr "]" " " | xargs go list -f '{{if not .Standard}}{{.ImportPath}}{{end}}'
