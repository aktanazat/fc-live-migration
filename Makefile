.PHONY: kernel rootfs build images up down migrate demo clean

BIN := bin

kernel:
	./guest/fetch-kernel.sh

rootfs:
	./guest/build-rootfs.sh

build:
	mkdir -p $(BIN)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o $(BIN)/hostd ./cmd/hostd
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o $(BIN)/observer ./cmd/observer
	CGO_ENABLED=0 go build -o $(BIN)/migratectl ./cmd/migratectl

images:
	docker build -f docker/Dockerfile -t fc-host .

up:
	docker compose -f docker/compose.yaml up -d --build

down:
	docker compose -f docker/compose.yaml down -v

migrate:
	$(BIN)/migratectl migrate --source http://127.0.0.1:8081 --target http://127.0.0.1:8082

demo:
	./scripts/demo.sh

clean:
	rm -rf $(BIN) artifacts
