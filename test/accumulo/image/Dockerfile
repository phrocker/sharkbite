FROM maven:3.9.11-eclipse-temurin-21 AS accumulo-build

RUN apt-get update \
 && apt-get install -y --no-install-recommends g++ make \
 && rm -rf /var/lib/apt/lists/*

ARG ACCUMULO_REVISION=1a716b2c1bb5762ead4b46d2bc4f53e13873b314
ARG ACCUMULO_SOURCE_SHA512=bf2cac6f7b5f759358ac306bba2342dc74c51bf63d70905da1004fc1eb0cfa66f6a73023ab250824a67f17d410e9b3351f097ddfd742bbc4b14f7a7feb6c4b2e
ARG ACCUMULO_ACCESS_REVISION=5d7a94492832121f507029d9d7e7627fd88e95ba
ARG ACCUMULO_ACCESS_SOURCE_SHA512=f519e992ff8f418f06a564c04313b93788460de6de7fee1dfd7503ad660149fdb3794dae61474369be1c671115f2ecda4d5b73d745cbd73972ed3d171875f243

ADD https://github.com/apache/accumulo/archive/${ACCUMULO_REVISION}.tar.gz /accumulo-source.tar.gz
ADD https://github.com/apache/accumulo-access/archive/${ACCUMULO_ACCESS_REVISION}.tar.gz /accumulo-access-source.tar.gz

RUN set -eux; \
    echo "${ACCUMULO_ACCESS_SOURCE_SHA512}  /accumulo-access-source.tar.gz" | sha512sum -c -; \
    mkdir /access; \
    tar -xzf /accumulo-access-source.tar.gz --strip-components=1 -C /access; \
    cd /access; \
    mvn --batch-mode --no-transfer-progress \
      versions:set \
      -DnewVersion=1.0.0-SNAPSHOT \
      -DgenerateBackupPoms=false; \
    mvn --batch-mode --no-transfer-progress \
      -pl modules/core -am \
      -DskipTests \
      -Dcheckstyle.skip \
      -Dspotbugs.skip \
      -Drat.skip \
      install; \
    echo "${ACCUMULO_SOURCE_SHA512}  /accumulo-source.tar.gz" | sha512sum -c -; \
    mkdir /source; \
    tar -xzf /accumulo-source.tar.gz --strip-components=1 -C /source; \
    cd /source; \
    mvn --batch-mode --no-transfer-progress \
      -pl assemble -am \
      -DskipTests \
      -Dcheckstyle.skip \
      -Dspotbugs.skip \
      -Drat.skip \
      package; \
    archive="$(find assemble/target -maxdepth 1 -type f -name 'accumulo-*-bin.tar.gz' -print -quit)"; \
    test -n "${archive}"; \
    mkdir /opt/accumulo; \
    tar -xzf "${archive}" --strip-components=1 -C /opt/accumulo

FROM apache/hadoop@sha256:0f79c18d27a2b15984868a41e245b09c98daa5638b573b4dbd848be650f05635 AS hadoop
FROM zookeeper@sha256:f5ce2c0274fd99c77bcb51505c748ac109fd5be36b4247b46c178ce59ea59e59 AS zookeeper

FROM eclipse-temurin:21-jdk-jammy

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends bash libsnappy1v5 procps; \
    rm -rf /var/lib/apt/lists/*

COPY --from=accumulo-build /opt/accumulo /opt/accumulo
COPY --from=hadoop /opt/hadoop /opt/hadoop
COPY --from=zookeeper /apache-zookeeper-3.9.5-bin /opt/zookeeper

ENV ACCUMULO_HOME=/opt/accumulo
ENV HADOOP_HOME=/opt/hadoop
ENV ZOOKEEPER_HOME=/opt/zookeeper
ENV PATH="${PATH}:/opt/accumulo/bin:/opt/hadoop/bin"

ENTRYPOINT ["/opt/accumulo/bin/accumulo"]
CMD ["help"]
