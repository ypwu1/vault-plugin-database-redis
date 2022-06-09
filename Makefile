vault_server := $(shell minikube ip)

go_files := $(shell find . -path '*/testdata' -prune -o -type f -name '*.go' -print)

.DEFAULT_GOAL := all
.PHONY := all start-docker configure-docker stop-docker test build coverage

bin/.coverage.out: $(go_files)
	@mkdir -p bin/
	go test -v ./... -coverpkg=$(shell go list ./... | xargs | sed -e 's/ /,/g') -coverprofile bin/.coverage.tmp
	@mv bin/.coverage.tmp bin/.coverage.out

test: bin/.coverage.out

coverage: bin/.coverage.out
	@go tool cover -html=bin/.coverage.out

bin/vault-plugin-database-redis: $(go_files)
	go build -trimpath -o ./bin/vault-plugin-database-redis ./cmd/vault-plugin-database-redis

bin/vault-plugin-database-redis_linux_amd64: $(go_files)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o ./bin/vault-plugin-database-redis_linux_amd64 ./cmd/vault-plugin-database-redis

build: bin/vault-plugin-database-redis_linux_amd64 bin/vault-plugin-database-redis

start-docker:
	docker-compose up --detach
	./bootstrap/redis-setup.sh -u $(TEST_USERNAME) -p $(TEST_PASSWORD) -db $(TEST_DB_NAME)
	# UI available at https://192.168.59.114:8443/

configure-docker: bin/vault-plugin-database-redis_linux_amd64
	docker-compose exec -- redis bash -c "echo 'ACL SETUSER admin on >password +@admin' | redis-cli"
	docker-compose exec -e VAULT_ADDR=http://$(vault_server):8200 vault vault login root
	docker-compose exec -e VAULT_ADDR=http://$(vault_server):8200 vault vault write sys/plugins/catalog/database/vault-redis-database-plugin command=vault-plugin-database-redis_linux_amd64 sha256=$(shell shasum -a 256 ./bin/vault-plugin-database-redis_linux_amd64 | awk '{print $$1}')
	docker-compose exec -e VAULT_ADDR=http://$(vault_server):8200 vault vault secrets enable database
	docker-compose exec -e VAULT_ADDR=http://$(vault_server):8200 vault vault write database/config/my-redis plugin_name="vault-redis-database-plugin" \
                                                                                            host="redis" port=6379 username="admin" password="password" \
                                                                                            allowed_roles="my-redis-*-role"
	docker-compose exec -e VAULT_ADDR=http://$(vault_server):8200 vault vault write database/roles/my-redis-admin-role db_name=my-redis \
                                                                            default_ttl="5m" max_ttl="1h" creation_statements='["+@admin"]'


stop-docker:
	cd bootstrap && docker-compose down

all: test build
