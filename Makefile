.PHONY: build install clean test fmt lint deploy-ec2

VERSION := 0.1.0
BINARY := ccvalet
BUILD_DIR := bin
EC2_HOST ?= ec2-dev

build:
	go build -o $(BUILD_DIR)/$(BINARY) ./cmd/ccvalet

install:
	go install ./cmd/ccvalet

clean:
	rm -rf $(BUILD_DIR)

test:
	go test -v ./...

fmt:
	go fmt ./...

lint:
	go vet ./...

# Deploy to EC2 (Ubuntu)
deploy-ec2:
	GOOS=linux GOARCH=amd64 go build -o $(BUILD_DIR)/$(BINARY)-linux-amd64 ./cmd/ccvalet
	scp $(BUILD_DIR)/$(BINARY)-linux-amd64 $(EC2_HOST):/tmp/$(BINARY)
	ssh $(EC2_HOST) 'sudo mv /tmp/$(BINARY) ~/.local/bin/$(BINARY) && sudo chmod +x ~/.local/bin/$(BINARY)'
	@echo "Deployed $(BINARY) to $(EC2_HOST):~/.local/bin/$(BINARY)"
