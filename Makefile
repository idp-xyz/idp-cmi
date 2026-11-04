# idp-cmi build and test entry points.
PG_IMAGE ?= public.ecr.aws/docker/library/postgres:17-alpine
PG_NAME  ?= idp-cmi-test-pg

.PHONY: build test vet test-integration e2e test-live pg-up pg-down

# Live model gate (ADR-0001 D15): the consolidation model against a real OpenAI-compatible endpoint, e.g.
#   IDP_LIVE_OPENAI_URL=http://127.0.0.1:8080/v1 IDP_LIVE_MODEL=qwen3.8-flash-next-iq3_xxs make test-live
test-live:
	go test -count=1 -v -timeout 10m ./tests/live/

e2e:
	bash deploy/e2e.sh

build:
	go build -o bin/ ./cmd/...

vet:
	gofmt -l . | (! grep .) && go vet ./...

test: vet
	go test -race ./...

test-integration: pg-up
	@PORT=$$(docker port $(PG_NAME) 5432/tcp | head -1 | sed 's/.*://'); \
	IDP_TEST_PG_OWNER_URL="postgres://postgres:test@127.0.0.1:$$PORT/postgres?sslmode=disable" \
	go test -count=1 -race -v -timeout 10m ./tests/integration/ ; status=$$?; \
	$(MAKE) --no-print-directory pg-down; exit $$status

pg-up:
	@docker rm -f $(PG_NAME) >/dev/null 2>&1 || true
	@docker run -d --name $(PG_NAME) -e POSTGRES_PASSWORD=test -p 127.0.0.1::5432 $(PG_IMAGE) >/dev/null
	@for i in $$(seq 1 60); do docker exec $(PG_NAME) pg_isready -h 127.0.0.1 -U postgres >/dev/null 2>&1 && break; sleep 0.5; done

pg-down:
	@docker rm -f $(PG_NAME) >/dev/null 2>&1 || true
