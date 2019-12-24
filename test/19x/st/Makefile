SMAC_VERSION=$(shell grep -A 1 "artifactId>standalone-mini-accumulo-cluster" pom.xml | tail -1 | sed -E 's/<version>//;s/<\/version>//;s/^\s+//')
DOCKER_SUB_VERSION=1
VERSION=$(SMAC_VERSION).$(DOCKER_SUB_VERSION)
BASE_NAME=mjwall/smac-docker-single
IMAGE_NAME=$(BASE_NAME):$(VERSION)

mkfile_path := $(abspath $(lastword $(MAKEFILE_LIST)))
mkfile_dir := $(shell dirname $(mkfile_path))

default: build

# dev targets

versions:
	@echo POM version $(SMAC_VERSION)
	@echo Docker image version $(VERSION)

clean:
	@echo Cleaning
	mvn clean 

package:
	@echo Creating jar
	mvn package

docker_build:
	@echo Building docker image
	@docker build -f Dockerfile -t ${IMAGE_NAME} .

docker_tag_latest:
	@docker tag ${IMAGE_NAME} ${BASE_NAME}:${SMAC_VERSION}-latest

build: clean package docker_build

# run jar targets

SMAC_ENV_FILE=$(HOME)/.smac-running-env
SMAC_PID=$(shell grep RUNNING_PID $(SMAC_ENV_FILE) 2>/dev/null | sed 's/.*=//')
SMAC_SHUTDOWN_PORT=$(shell grep SHUTDOWN_PORT $(SMAC_ENV_FILE) 2>/dev/null | sed 's/.*=//')
SMAC_RUNNING=$(shell kill -0 $(SMAC_PID) 1>&2 2>/dev/null; echo $$?)

DOWNLOADS_DIR=$(mkfile_dir)/target
SMAC_FILE=$(DOWNLOADS_DIR)/standalone-mac-shaded-$(SMAC_VERSION).jar
# maven filtered version
SMAC_LOGGER=$(DOWNLOADS_DIR)/classes/smac_logger.xml

ensure_smac_env_file:
ifneq ("$(wildcard $(SMAC_ENV_FILE))","")
	@echo ${SMAC_ENV_FILE} exists
else
	@echo ${SMAC_ENV_FILE} does not exist; exit 1
endif

ensure_running: ensure_smac_env_file
ifeq ($(SMAC_RUNNING),0)
	@echo SMAC running
else
	@echo SMAC not running; exit 1
endif

ensure_not_running: 
ifeq ($(SMAC_RUNNING),0)
	@echo Running; exit 1
else
	@echo SMAC not running
endif

run: ensure_not_running
	@echo Running from jar
	java -jar ${SMAC_FILE}

run_d: ensure_not_running
	# don't start a shell and use the smac_logger.xml
	@echo Running from jar in background
	nohup java -DstartShell=false -Dlog4jConfig=${SMAC_LOGGER} -jar ${SMAC_FILE} > target/nohup.out 2>&1 &

stop: ensure_running
	@echo Stopping
	curl telnet://localhost:${SMAC_SHUTDOWN_PORT}

run_ashell: ensure_running
	@echo Launching accumulo shell
	java -cp ${SMAC_FILE} com.mjwall.accumulo.SmacAccumuloShell

run_zkcli: ensure_running
	@echo Launching zkcli
	java -cp ${SMAC_FILE} com.mjwall.accumulo.SmacZookeeperCli

# run docker targets

DOCKER_INSTANCE_ID := $(shell docker ps | grep smac-docker-single | cut -d ' ' -f 1)

ensure_not_running_docker:
ifeq ("${DOCKER_INSTANCE_ID}x", "x")
	@echo Not running...
else
	@echo ERROR: already running; exit 1
endif

ensure_running_docker:
ifeq ("${DOCKER_INSTANCE_ID}x", "x")
	@echo ERROR: not running; exit 1
else
	@echo Running...
endif

# run container with default entrypoint which starts the smac
# use --net=host if you want to hit the accumulo monitor
# or remote debug something.  Stays in the shell
docker_run: ensure_not_running_docker
	docker run --rm -it -e ACCUMULO_START_SHELL=true --network=host ${IMAGE_NAME}

# run container in the background
# also show how to override stuff with environment variables
# see sample-env.sh and start-smac.sh
docker_run_d: ensure_not_running_docker
	docker run --rm --network=host -d -e "ACCUMULO_SHUTDOWN_PORT=4447" ${IMAGE_NAME}

docker_stop: ensure_running_docker
	docker stop ${DOCKER_INSTANCE_ID}

# attach to a running container
docker_attach: ensure_running_docker
	docker exec -it ${DOCKER_INSTANCE_ID} /bin/sh

# attach to a running container and run an accumulo shell
docker_ashell: ensure_running_docker
	docker exec -it ${DOCKER_INSTANCE_ID} /smac-bin/start-accumulo-shell.sh

# attach to a running container and run a zkCli
docker_zkcli: ensure_running_docker
	docker exec -it ${DOCKER_INSTANCE_ID} /smac-bin/start-zookeeper-cli.sh

