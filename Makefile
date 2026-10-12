#
# Licensed to the Apache Software Foundation (ASF) under one
# or more contributor license agreements.  See the NOTICE file
# distributed with this work for additional information
# regarding copyright ownership.  The ASF licenses this file
# to you under the Apache License, Version 2.0 (the
# "License"); you may not use this file except in compliance
# with the License.  You may obtain a copy of the License at
#
#   https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing,
# software distributed under the License is distributed on an
# "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
# KIND, either express or implied.  See the License for the
# specific language governing permissions and limitations
# under the License.
#

SHELL := /bin/bash

THRIFT_VERSION        := 0.17.0
THRIFT                ?= thrift
THRIFT_IDL            := internal/thrift/idl
THRIFT_OUT            := internal/thrift/gen
THRIFT_PACKAGE_PREFIX := github.com/phrocker/shoal-oss/internal/thrift/gen/
IMAGE                 ?= shoal-embed:dev
GATEWAY_IMAGE         ?= shoal-gateway:dev
EFFECTS_SSH_IMAGE     ?= shoal-effects-ssh:dev
VERSION               ?= dev
REVISION              ?= $(shell git rev-parse HEAD)
CREATED               ?= $(shell git show -s --format=%cI HEAD)

GOOS := $(shell go env GOOS)
ifeq ($(GOOS),windows)
CAPI_LIBRARY := shoal.dll
CAPI_INTERMEDIATE := shoal-cgo.dll
else ifeq ($(GOOS),darwin)
CAPI_LIBRARY := libshoal.dylib
CAPI_INTERMEDIATE := shoal-cgo.dylib
else
CAPI_LIBRARY := libshoal.so
CAPI_INTERMEDIATE := shoal-cgo.so
endif

# Generate the top-level services shoal uses. The compiler's recursive
# mode follows their vendored includes and emits the shared data packages.
THRIFT_FILES := \
	$(THRIFT_IDL)/tabletscan.thrift \
	$(THRIFT_IDL)/tabletingest.thrift \
	$(THRIFT_IDL)/tabletmgmt.thrift \
	$(THRIFT_IDL)/compaction-coordinator.thrift

.PHONY: all
all: build

.PHONY: thrift-check
thrift-check:
	@command -v "$(THRIFT)" >/dev/null 2>&1 || { \
	  echo "thrift compiler not found: $(THRIFT)"; \
	  echo "install Apache Thrift $(THRIFT_VERSION) or set THRIFT=/path/to/thrift"; \
	  exit 1; \
	}
	@v=$$("$(THRIFT)" --version | awk '{print $$3}'); \
	  test "$$v" = "$(THRIFT_VERSION)" || { \
	    echo "expected thrift $(THRIFT_VERSION), got $$v"; \
	    exit 1; \
	  }

.PHONY: thrift-gen
thrift-gen: thrift-check
	rm -rf $(THRIFT_OUT)
	mkdir -p $(THRIFT_OUT)
	for f in $(THRIFT_FILES); do \
	  "$(THRIFT)" -r -I $(THRIFT_IDL) \
	    --gen go:package_prefix=$(THRIFT_PACKAGE_PREFIX) \
	    -out $(THRIFT_OUT) $$f || exit 1; \
	done
	# Drop standalone -remote debug CLIs; they target a newer apache/thrift
	# Go runtime API and we don't use them.
	find $(THRIFT_OUT) -type d -name '*-remote' -exec rm -rf {} +
	# Thrift derives the Go package from the hyphenated IDL basename, which is
	# not a valid Go identifier. Accumulo's Java generator does not need a Go
	# namespace, so normalize this one package after generation.
	if test -d $(THRIFT_OUT)/compaction-coordinator; then \
	  sed -i 's/^package compaction-coordinator$$/package compactioncoordinator/' \
	    $(THRIFT_OUT)/compaction-coordinator/*.go; \
	  for f in $(THRIFT_OUT)/compaction-coordinator/*; do \
	    b=$$(basename "$$f" | sed 's/compaction-coordinator/compactioncoordinator/g'); \
	    mv "$$f" "$(THRIFT_OUT)/compaction-coordinator/$$b"; \
	  done; \
	  mv $(THRIFT_OUT)/compaction-coordinator $(THRIFT_OUT)/compactioncoordinator; \
	fi
	$(MAKE) patch-thrift-nil-binary
	$(MAKE) _normalize-tabletmgmt

.PHONY: _normalize-tabletmgmt
_normalize-tabletmgmt:
	@if test -d $(THRIFT_OUT)/tabletmgmt; then \
	  find $(THRIFT_OUT)/tabletmgmt -name '*.go' -print0 | \
	    xargs -0 -I{} sh -c '\
	      f="$$1"; \
	      sed -i -e "s/[[:space:]]\\+$$//" "$$f"; \
	      awk '"'"'{ lines[NR]=$$0; if ($$0 != "") last=NR } \
	        END { for (i=1; i<=last; i++) print lines[i] }'"'"' \
	        "$$f" > "$$f.tmp" && mv "$$f.tmp" "$$f"; \
	    ' sh {}; \
	fi

.PHONY: thrift-verify
thrift-verify: validate thrift-check
	@test -d "$(THRIFT_OUT)" || { \
	  echo "generated Thrift bindings not found at $(THRIFT_OUT); run make thrift-gen"; \
	  exit 1; \
	}
	@tmp=$$(mktemp -d); \
	  trap 'rm -rf "$$tmp"' EXIT; \
	  cp -R $(THRIFT_OUT) "$$tmp/gen"; \
	  $(MAKE) --no-print-directory thrift-gen; \
	  if ! diff -ru --strip-trailing-cr "$$tmp/gen" $(THRIFT_OUT); then \
	    echo "generated Thrift bindings are stale; run make thrift-gen"; \
	    exit 1; \
	  fi; \
	  echo "generated Thrift bindings match the vendored IDLs"

# Patch generated writeFieldN functions that write a struct-pointer field
# unconditionally. Java treats absent struct fields as "no value"; the Go
# generator emits an empty struct in their place, which Accumulo's server
# typically interprets as "configured but malformed" → NPE.
#
# We do this generically: any writeField that contains `p.<Field>.Write(ctx, oprot)`
# gets a `if p.<Field> == nil { return nil }` guard inserted at the top.
# Same logic for binary fields (`p.<Field>` passed to WriteBinary) — we
# only guard the explicitly-listed ones we know need it, since not all
# empty binary fields are equivalent to absent on the Java side.
.PHONY: patch-thrift-nil-binary
patch-thrift-nil-binary:
	@$(MAKE) --no-print-directory _patch-binary-fields
	@$(MAKE) --no-print-directory _patch-struct-fields

# Patch TKeyExtent infinite bounds and Manager waitForFlush unbounded rows.
.PHONY: _patch-binary-fields
_patch-binary-fields:
	@f=$(THRIFT_OUT)/data/data.go; \
	test -f $$f || { echo "$$f not found; run thrift-gen first"; exit 1; }; \
	if grep -q 'PATCH (shoal): skip endRow' $$f; then \
	  echo "_patch-binary-fields: already applied"; \
	else \
	  awk 'BEGIN { p=0 } \
	       /^func \(p \*TKeyExtent\) writeField2\(/ { p=1; print; next } \
	       /^func \(p \*TKeyExtent\) writeField3\(/ { p=2; print; next } \
	       p==1 && /WriteFieldBegin\(ctx, "endRow"/     { print "  // PATCH (shoal): skip endRow when nil so wire matches Java\047s \"infinite endRow\" semantics."; print "  if p.EndRow == nil { return nil }"; p=0 } \
	       p==2 && /WriteFieldBegin\(ctx, "prevEndRow"/ { print "  // PATCH (shoal): null prevEndRow = \"infinite prev\" (start of table)."; print "  if p.PrevEndRow == nil { return nil }"; p=0 } \
	       { print }' $$f > $$f.tmp && mv $$f.tmp $$f; \
	  echo "_patch-binary-fields: applied"; \
	fi
	@f=$(THRIFT_OUT)/manager/manager.go; \
	test -f $$f || { echo "$$f not found; run thrift-gen first"; exit 1; }; \
	if grep -q 'PATCH (shoal): nil startRow = unbounded flush start' $$f; then \
	  echo "_patch-binary-fields: waitForFlush already applied"; \
	else \
	  awk 'BEGIN { p=0 } \
	       /^func \(p \*ManagerClientServiceWaitForFlushArgs\) writeField4\(/ { p=1; print; next } \
	       /^func \(p \*ManagerClientServiceWaitForFlushArgs\) writeField5\(/ { p=2; print; next } \
	       p==1 && /WriteFieldBegin\(ctx, "startRow"/ { print "  // PATCH (shoal): nil startRow = unbounded flush start."; print "  if p.StartRow == nil { return nil }"; p=0 } \
	       p==2 && /WriteFieldBegin\(ctx, "endRow"/   { print "  // PATCH (shoal): nil endRow = unbounded flush end."; print "  if p.EndRow == nil { return nil }"; p=0 } \
	       { print }' $$f > $$f.tmp && mv $$f.tmp $$f; \
	  echo "_patch-binary-fields: waitForFlush applied"; \
	fi
	@f=$(THRIFT_OUT)/data/data.go; \
	test -f $$f || { echo "$$f not found; run thrift-gen first"; exit 1; }; \
	if grep -q 'PATCH (shoal): nil condition value = absent column' $$f; then \
	  echo "_patch-binary-fields: TCondition already applied"; \
	else \
	  awk 'BEGIN { p=0 } \
	       /^func \(p \*TCondition\) writeField6\(/ { p=1; print; next } \
	       p==1 && /WriteFieldBegin\(ctx, "val"/ { print "  // PATCH (shoal): nil condition value = absent column."; print "  if p.Val == nil { return nil }"; p=0 } \
	       { print }' $$f > $$f.tmp && mv $$f.tmp $$f; \
	  echo "_patch-binary-fields: TCondition applied"; \
	fi; \
	awk '{ lines[NR]=$$0; if ($$0 != "") last=NR } \
	     END { for (i=1; i<=last; i++) print lines[i] }' \
	     $$f > $$f.tmp && mv $$f.tmp $$f

# Generic struct-pointer guard: any writeFieldN across all generated .go
# files where the body contains `p.<Field>.Write(ctx, oprot)` gets the
# nil-skip guard injected. Idempotent: the marker comment prevents re-patching.
.PHONY: _patch-struct-fields
_patch-struct-fields:
	@find $(THRIFT_OUT) -name '*.go' -print0 | xargs -0 -I{} sh -c '\
	  f="$$1"; \
	  awk -v file="$$f" '"'"' \
	    BEGIN { changed=0 } \
	    /^func \(p \*[A-Za-z0-9_]+\) writeField[0-9]+\(ctx context\.Context, oprot thrift\.TProtocol\) \(err error\) \{$$/ { \
	      header=$$0; getline next1; getline next2; getline next3; \
	      if (next3 ~ /p\.[A-Z][A-Za-z0-9_]*\.Write\(ctx, oprot\)/ && next1 !~ /PATCH \(shoal\)/) { \
	        match(next3, /p\.([A-Z][A-Za-z0-9_]*)\.Write/, m); \
	        print header; \
	        print "  // PATCH (shoal): null struct-pointer field = absent on wire."; \
	        print "  if p." m[1] " == nil { return nil }"; \
	        print next1; print next2; print next3; \
	        changed=1; next; \
	      } else { print header; print next1; print next2; print next3; next; } \
	    } \
	    { print } \
	    END { if (changed) print "_patch-struct-fields: patched " file > "/dev/stderr" } \
	  '"'"' "$$f" > "$$f.tmp" && mv "$$f.tmp" "$$f"; \
	' _ {}

.PHONY: build
build:
	go build ./...

.PHONY: container-build
container-build:
	docker build \
	  --file Dockerfile.shoal-embed \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg REVISION=$(REVISION) \
	  --build-arg CREATED=$(CREATED) \
	  --tag $(IMAGE) \
	  .

.PHONY: container-smoke
container-smoke:
	IMAGE=$(IMAGE) VERSION=$(VERSION) REVISION=$(REVISION) CREATED=$(CREATED) \
	  bash test/container/shoal-embed-smoke.sh

# The effects gateway image (#391), which the chart's effectsGateways entries run.
.PHONY: gateway-container-build
gateway-container-build:
	docker build \
	  --file Dockerfile.shoal-gateway \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg REVISION=$(REVISION) \
	  --build-arg CREATED=$(CREATED) \
	  --tag $(GATEWAY_IMAGE) \
	  .

.PHONY: gateway-container-smoke
gateway-container-smoke:
	IMAGE=$(GATEWAY_IMAGE) VERSION=$(VERSION) REVISION=$(REVISION) CREATED=$(CREATED) \
	  bash test/container/shoal-gateway-smoke.sh

.PHONY: effects-ssh-container-build
effects-ssh-container-build:
	docker build \
	  --file Dockerfile.shoal-effects-ssh \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg REVISION=$(REVISION) \
	  --build-arg CREATED=$(CREATED) \
	  --tag $(EFFECTS_SSH_IMAGE) \
	  .

.PHONY: effects-ssh-container-smoke
effects-ssh-container-smoke:
	IMAGE=$(EFFECTS_SSH_IMAGE) VERSION=$(VERSION) REVISION=$(REVISION) CREATED=$(CREATED) \
	  bash test/container/shoal-effects-ssh-smoke.sh

.PHONY: capi
capi:
	mkdir -p bin/capi
	go build -buildmode=c-shared -o bin/capi/$(CAPI_INTERMEDIATE) ./cmd/shoal-capi
	mv bin/capi/$(CAPI_INTERMEDIATE) bin/capi/$(CAPI_LIBRARY)
	rm -f bin/capi/shoal-cgo.h
	cp capi/include/shoal.h capi/include/shoal_types.h bin/capi/

.PHONY: python-test
python-test:
	cd python && PYTHONPATH=src python -m unittest discover -s tests -v

.PHONY: python-dist
python-dist:
	python python/scripts/build_release.py

.PHONY: python-dist-verify
python-dist-verify:
	python python/scripts/verify_release.py

.PHONY: python-preview-macos
python-preview-macos:
	python python/scripts/build_preview.py --platform-tag macosx_11_0_x86_64 --dist build/cross-platform-artifacts/wheel-preview-darwin-amd64 --allow-dirty
	python python/scripts/verify_preview.py --dist build/cross-platform-artifacts/wheel-preview-darwin-amd64
	python python/scripts/build_preview.py --platform-tag macosx_11_0_arm64 --dist build/cross-platform-artifacts/wheel-preview-darwin-arm64 --allow-dirty
	python python/scripts/verify_preview.py --dist build/cross-platform-artifacts/wheel-preview-darwin-arm64

.PHONY: cross-platform-artifacts
cross-platform-artifacts:
	python scripts/cross_platform_artifacts.py

.PHONY: python-release-validate
python-release-validate: python-test python-dist python-dist-verify
	go test -tags=shoal_capi_test ./cmd/shoal-capi

.PHONY: docs-validate
docs-validate:
	python docs/test_validate_sharkbite_matrix.py
	python docs/validate_sharkbite_matrix.py

.PHONY: module-path-verify
module-path-verify:
	@test "$$(GOWORK=off go list -m -f '{{.Path}}')" = "github.com/phrocker/shoal-oss" || { \
	  echo "go.mod must declare module github.com/phrocker/shoal-oss"; \
	  exit 1; \
	}
	@if git grep -n -E 'github\.com/phrocker/shoal([^[:alnum:]_-]|$$)' -- .; then \
	  echo "stale pre-OSS module references found"; \
	  exit 1; \
	fi
	@echo "module path and repository references are canonical"

.PHONY: validate
validate: docs-validate module-path-verify

.PHONY: test
test:
	go test ./...

.PHONY: test-hdfs
test-hdfs:
	@set -e; \
		cleanup() { status=$$?; if [ $$status -ne 0 ]; then docker compose -f test/hdfs/docker-compose.yml logs --no-color || true; fi; docker compose -f test/hdfs/docker-compose.yml down -v || true; exit $$status; }; \
		trap cleanup EXIT; \
		docker compose -f test/hdfs/docker-compose.yml up -d; \
		bash test/hdfs/wait.sh; \
		HADOOP_CONF_DIR=$(CURDIR)/test/hdfs/client-conf \
		HADOOP_USER_NAME=shoal \
		SHOAL_HDFS_INTEGRATION=1 \
		go test -tags=integration -count=1 -v ./internal/storage/hdfs

.PHONY: test-accumulo-static
test-accumulo-static:
	python test/accumulo/harness.py validate
	python -m unittest -v test/accumulo/test_harness.py
	go test ./internal/conformance ./cmd/shoal-conformance

.PHONY: conformance-replay
conformance-replay:
	mkdir -p bin
	go build -o bin/shoal-conformance ./cmd/shoal-conformance
	bin/shoal-conformance -mode replay

.PHONY: conformance-live
conformance-live:
	mkdir -p bin
	go build -o bin/shoal-conformance ./cmd/shoal-conformance
	bin/shoal-conformance -mode live

.PHONY: conformance-live-tserver
conformance-live-tserver:
	mkdir -p bin
	go build -o bin/shoal-conformance ./cmd/shoal-conformance
	bin/shoal-conformance -mode live -required tserver,scanserver,client

.PHONY: conformance-live-compactor
conformance-live-compactor:
	mkdir -p bin
	go build -o bin/shoal-conformance ./cmd/shoal-conformance
	bin/shoal-conformance -mode live -required compactor,promotion

.PHONY: accumulo-up
accumulo-up:
	python test/accumulo/harness.py up

.PHONY: accumulo-smoke
accumulo-smoke:
	python test/accumulo/harness.py smoke

.PHONY: accumulo-down
accumulo-down:
	python test/accumulo/harness.py down

.PHONY: test-accumulo
test-accumulo:
	python test/accumulo/harness.py test

.PHONY: vet
vet:
	go vet ./...

.PHONY: clean
clean:
	rm -rf $(THRIFT_OUT) bin python/build python/dist \
	  python/src/sharkbite/.libs python/src/*.egg-info
