.DEFAULT_GOAL := install
install:
	go build -o $$GOPATH/bin/slack-shell
