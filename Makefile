.PHONY: build test fmt format-check lint vet typecheck bundle-slim bundle-full

build:
	pnpm build

test:
	pnpm test

fmt:
	pnpm format

format-check:
	pnpm format:check

lint vet:
	pnpm lint

typecheck:
	pnpm typecheck

bundle-slim:
	pnpm bundle:slim

bundle-full:
	pnpm bundle:full
