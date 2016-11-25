all: push

# 0.0 shouldn't clobber any release builds
TAG = 0.8.3-fatalpatch
PREFIX = quay.io/nordstrom/nginx-ingress-controller

REPO_INFO=$(shell git config --get remote.origin.url)

ifndef VERSION
  VERSION := git-$(shell git rev-parse --short HEAD)
endif

controller: controller.go clean
	CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -ldflags \
		"-s -w -X main.version=${VERSION} -X main.gitRepo=${REPO_INFO}" \
		-o nginx-ingress-controller

container: controller
	docker build -t $(PREFIX):$(TAG) . \
	--build-arg http_proxy=$(HTTP_PROXY) --build-arg https_proxy=$(HTTPS_PROXY)

push: container
	gcloud docker push $(PREFIX):$(TAG)

clean:
	rm -f nginx-ingress-controller
