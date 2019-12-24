FROM library/openjdk:8-jdk-alpine

LABEL maintainer="mjwall@gmail.org"
LABEL description="Standalone Mini Accumulo Cluster running in a container"
LABEL run="docker run --rm -d --net=host mjwall/smac-docker-single:<tag>"
LABEL source="https://gitlab.com/mjwall/standalone-mac"

RUN apk update && \
    apk upgrade && \
    apk add curl && \
    rm -rf /var/cache/apk/*

RUN mkdir /downloads && mkdir /smac-bin
COPY target/standalone-mac-shaded-*.jar /downloads/
COPY target/classes/smac_logger.xml /smac-bin/
COPY src/main/bin/* /smac-bin/

CMD ["/smac-bin/start-smac.sh"]
