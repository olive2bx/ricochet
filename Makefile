# ricochet — tâches de développement, CI et release.
# Tout passe par l'outil `go` : aucune autre dépendance n'est requise (hors
# `make` lui-même). Lancer `make help` pour la liste des cibles.

BINARY   := ricochet
PKG      := ./cmd/ricochet
DIST     := dist
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.version=$(VERSION)
GOFLAGS_BUILD := -trimpath -ldflags "$(LDFLAGS)"

# Outils d'analyse lancés via `go run` (versions figées, rien à installer).
STATICCHECK := honnef.co/go/tools/cmd/staticcheck@2025.1.1
GOVULNCHECK := golang.org/x/vuln/cmd/govulncheck@v1.1.4

# Cibles de cross-compilation (GOOS/GOARCH).
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

export CGO_ENABLED ?= 0

.DEFAULT_GOAL := build

.PHONY: help
help: ## Affiche cette aide
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

## --- Build ------------------------------------------------------------------

.PHONY: build
build: ## Compile le binaire (sans SQLite)
	go build $(GOFLAGS_BUILD) -o $(BINARY)$(EXE) $(PKG)

.PHONY: build-sqlite
build-sqlite: ## Compile le binaire avec l'historique SQLite (-tags sqlite)
	go build $(GOFLAGS_BUILD) -tags sqlite -o $(BINARY)$(EXE) $(PKG)

.PHONY: run
run: build ## Lance le proxy (ARGS="--rules rules.txt ...")
	./$(BINARY)$(EXE) $(ARGS)

.PHONY: cross
cross: ## Compile toutes les plateformes, avec et sans SQLite, dans dist/
	@mkdir -p $(DIST)
	@set -e; for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=""; [ "$$os" = windows ] && ext=".exe"; \
		for variant in "" sqlite; do \
			suffix=""; [ -n "$$variant" ] && suffix="_$$variant"; \
			out="$(DIST)/$(BINARY)_$(VERSION)_$${os}_$${arch}$${suffix}$${ext}"; \
			echo "  build $$out"; \
			GOOS=$$os GOARCH=$$arch go build $(GOFLAGS_BUILD) -tags "$$variant" -o "$$out" $(PKG); \
		done; \
	done

.PHONY: dist
dist: clean-dist cross checksums ## Prépare les artefacts de release (binaires + SHA256SUMS)

.PHONY: checksums
checksums:
	@cd $(DIST) && if command -v sha256sum >/dev/null; then sha256sum $(BINARY)_* > SHA256SUMS; \
		else shasum -a 256 $(BINARY)_* > SHA256SUMS; fi
	@echo "  $(DIST)/SHA256SUMS"

## --- Qualité ----------------------------------------------------------------

.PHONY: fmt
fmt: ## Formate le code (gofmt)
	gofmt -s -w .

.PHONY: fmt-check
fmt-check: ## Échoue si du code n'est pas formaté
	@out=$$(gofmt -s -l .); if [ -n "$$out" ]; then echo "fichiers non formatés :"; echo "$$out"; exit 1; fi

.PHONY: vet
vet: ## go vet, avec et sans le tag sqlite
	go vet ./...
	go vet -tags sqlite ./...

.PHONY: lint
lint: ## staticcheck, avec et sans le tag sqlite
	go run $(STATICCHECK) ./...
	go run $(STATICCHECK) -tags sqlite ./...

.PHONY: vuln
vuln: ## govulncheck (dépendances et bibliothèque standard)
	go run $(GOVULNCHECK) ./...
	go run $(GOVULNCHECK) -tags sqlite ./...

.PHONY: tidy-check
tidy-check: ## Échoue si go.mod / go.sum ne sont pas à jour
	go mod tidy
	@git diff --exit-code -- go.mod go.sum || (echo "go.mod/go.sum ne sont pas à jour : lancer 'go mod tidy'"; exit 1)

## --- Tests ------------------------------------------------------------------

.PHONY: test
test: ## Tests unitaires et de bout en bout
	go test ./...

.PHONY: test-sqlite
test-sqlite: ## Tests avec le tag sqlite
	go test -tags sqlite ./...

.PHONY: test-race
test-race: ## Tests avec le détecteur de courses (nécessite CGO)
	CGO_ENABLED=1 go test -race ./...
	CGO_ENABLED=1 go test -race -tags sqlite ./...

.PHONY: cover
cover: ## Couverture de tests (coverage.out + résumé)
	go test -tags sqlite -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

.PHONY: ci
ci: fmt-check tidy-check vet lint test test-sqlite ## Enchaîne les vérifications de la CI

## --- Nettoyage --------------------------------------------------------------

.PHONY: clean-dist
clean-dist:
	rm -rf $(DIST)

.PHONY: clean
clean: clean-dist ## Supprime les binaires et artefacts
	rm -f $(BINARY) $(BINARY).exe coverage.out
