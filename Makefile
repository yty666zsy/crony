# Go makefile

#export env
#basic information
ProjectAdmin := "cronyadmin"
ProjectNode := "cronynode"

PROJECTBASE 	:= $(shell pwd)
PROJECTBIN 	:= $(PROJECTBASE)/bin
AdminConf := "$(PROJECTBIN)/admin"
NodeConf := "$(PROJECTBIN)/node"
TIMESTAMP   := $(shell /bin/date "+%F %T")

#change to deploy environment
AdminFile := ./admin/cmd/main.go
NodeFile := ./node/cmd/main.go
WebFile := ./admin/web

#compile ldflags
LDFLAGS		:= -s -w \
			   -X 'main.BuildGitBranch=$(shell git describe --all)' \
			   -X 'main.BuildGitRev=$(shell git rev-list --count HEAD)' \
			   -X 'main.BuildGitCommit=$(shell git rev-parse HEAD)' \
			   -X 'main.BuildDate=$(shell /bin/date "+%F %T")'


linux-dev: clean install-web build-web
	@echo "install linux amd64 dev version"
	@if [ ! -d $(AdminConf)/logs ]; then \
        mkdir -p $(AdminConf)/logs; \
    fi
	@if [ ! -d $(NodeConf)/logs ]; then \
        mkdir -p $(NodeConf)/logs; \
    fi
	@cp -r ./admin/conf $(AdminConf)
	@cp -r ./node/conf $(NodeConf)
	@echo "building project cronyadmin..."
	@CGO_ENABLED=0 GOOS=linux  GOARCH=amd64 go build -v -o $(PROJECTBIN)/$(ProjectAdmin) $(AdminFile)
	@echo "building project cronynode..."
	@CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -v -o $(PROJECTBIN)/$(ProjectNode) $(NodeFile)
	@chmod +x $(PROJECTBIN)/$(ProjectAdmin)
	@chmod +x $(PROJECTBIN)/$(ProjectNode)

	@mv $(WebFile)/dist bin/
	@echo "build success."

install-web:
	@echo "install web node_modules..."
	cd $(WebFile)&&npm install

build-web:
	@echo "building web..."
	cd $(WebFile)&&yarn build

run-web:
	@echo "running web..."
	cd $(WebFile) &&yarn serve

local-dev: clean install-web
	@echo "install local dev version"
	@go mod tidy
	@if [ ! -d $(NodeConf)/logs ]; then \
        mkdir -p $(NodeConf)/logs; \
    fi
	@if [ ! -d $(AdminConf)/logs ]; then \
            mkdir -p $(AdminConf)/logs; \
    fi
	@cp -r ./admin/conf $(AdminConf)
	@cp -r ./node/conf $(NodeConf)
	@echo "building project cronyadmin..."
	@CGO_ENABLED=0 go build -v  -o $(PROJECTBIN)/$(ProjectAdmin) $(AdminFile)
	@echo "building project cronynode..."
	@CGO_ENABLED=0 go build -v  -o $(PROJECTBIN)/$(ProjectNode) $(NodeFile)
	@chmod +x $(PROJECTBIN)/$(PROJECTNAME)
	@chmod +x $(PROJECTBIN)/$(ProjectNode)
	@echo "build success."

gitpush: clean fmt
	git add .
	git commit -m "$(m) changed at $(TIMESTAMP)"
	git push
fmt:
	@go fmt $(PROJECTBASE)/...
	@echo "hello"
	@go mod tidy

# 一键门禁：与 gate.ps1 同序（格式 → 构建 → vet → 带标签 vet → 单测）。
# 带标签的 vet 必须单独跑：普通 go vet ./... **不覆盖**带构建标签的验收代码。
gate:
	@echo "== gofmt"
	@out="$$(gofmt -l admin common node acceptance)"; \
	if [ -n "$$out" ]; then echo "$$out"; echo "以上文件未 gofmt（go fmt admin common node acceptance）"; exit 1; fi
	@echo "== go build ./..."; go build ./...
	@echo "== go vet ./..."; go vet ./...
	@echo "== go vet -tags acceptance ./acceptance/"; go vet -tags acceptance ./acceptance/
	@echo "== go test ./..."; go test ./... -count=1
	@echo "gate ok"

# 在 gate 之上追加黑盒验收（需被测系统在跑 + 已配置 CRONY_ACC_* 变量）。
gate-acceptance: gate
	@echo "== go test -tags acceptance ./acceptance/"; go test -tags acceptance ./acceptance/ -count=1 -timeout 900s

clean:
	@#echo $(PROJECTBIN)
	@rm -rf $(PROJECTBIN)/* &>/dev/null
depend:
	go mod download
gitpull: fmt
	git add .
	git commit -m "$(m) changed at $(TIMESTAMP)"
	git pull
.PHONY: fmt clean git gate gate-acceptance