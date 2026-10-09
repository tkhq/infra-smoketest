.PHONY: build test integration generate fmt vet check manifests terraform-check

build:
	CGO_ENABLED=0 go build -trimpath -o bin/infra-smoketest ./cmd/infra-smoketest

test:
	go test -race -count=1 ./...

integration:
	go test -race -count=1 -tags=integration ./internal/controller/...

generate:
	controller-gen object paths=./api/...
	controller-gen crd:crdVersions=v1,generateEmbeddedObjectMeta=true paths=./api/... output:crd:dir=config/crd

fmt:
	gofmt -w api cmd internal

vet:
	go vet ./...

manifests:
	kustomize build config/default >/dev/null
	kustomize build config/monitoring >/dev/null
	kustomize build config/aws-pod-identity-webhook >/dev/null
	kustomize build examples >/dev/null

terraform-check:
	terraform fmt -check -recursive config/terraform
	terraform -chdir=config/terraform/aws init -backend=false -input=false -lockfile=readonly
	terraform -chdir=config/terraform/aws validate
	terraform -chdir=config/terraform/aws test
	terraform -chdir=config/terraform/aws/examples/from-repo init -backend=false -input=false -lockfile=readonly
	terraform -chdir=config/terraform/aws/examples/from-repo validate

check: vet test integration manifests terraform-check
	@test -z "$$(gofmt -l api cmd internal)"
	actionlint
	shellcheck scripts/*.sh
