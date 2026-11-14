.PHONY: test lint image smoke
VERSION ?= dev

test:
	go vet ./...
	go test -race -count=1 ./...

lint:
	gofmt -l . | (! grep .)
	helm lint deploy/helm/teams-a2a-gateway -f deploy/helm/teams-a2a-gateway/ci/kagent-values.yaml
	shellcheck deploy/azure/*.sh deploy/teams-app/*.sh deploy/kind/*.sh

image:
	docker build --build-arg VERSION=$(VERSION) -t teams-a2a-gateway:$(VERSION) .

smoke:
	deploy/kind/smoke.sh
