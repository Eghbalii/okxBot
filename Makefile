.PHONY: go-build go-lint go-test py-venv py-install py-test py-train py-serve ingestor trader fmt

go-build:
	cd go-engine && go build ./...

go-lint:
	cd go-engine && go vet ./... && gofmt -l .

go-test:
	cd go-engine && go test ./...

ingestor:
	cd go-engine && go run ./cmd/ingestor

trader:
	cd go-engine && go run ./cmd/trader

py-venv:
	cd rl-service && python3 -m venv .venv

py-install:
	cd rl-service && .venv/bin/pip install -r requirements.txt

py-test:
	cd rl-service && .venv/bin/python -m pytest tests/ -q

py-train:
	cd rl-service && .venv/bin/python -m rl_service.train --config configs/config.yaml

py-serve:
	cd rl-service && .venv/bin/uvicorn rl_service.serve.api:app --reload --port 8000

fmt:
	cd go-engine && gofmt -w .
	cd rl-service && .venv/bin/black . && .venv/bin/ruff check --fix .
