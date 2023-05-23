# Licensed to the Apache Software Foundation (ASF) under one
# or more contributor license agreements. See the NOTICE file
# distributed with this work for additional information
# regarding copyright ownership. The ASF licenses this file
# to you under the Apache License, Version 2.0 (the
# "License"); you may not use this file except in compliance
# with the License. You may obtain a copy of the License at
#
#   http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing,
# software distributed under the License is distributed on an
# "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
# KIND, either express or implied. See the License for the
# specific language governing permissions and limitations
# under the License.
#

ARG BASE_ALPINE_IMAGE="alpine:3.16"

# Build image
FROM ${BASE_ALPINE_IMAGE} AS build
LABEL maintainer="Marc Parisi <phrocker@apache.org>"

ARG SHARKBITE_VERSION
ARG UID=1000
ARG GID=1000

# SHARKBITE_OPTIONS will be passed directly to cmake
# use it to define cmake options (e.g. -DENABLE_AWS=ON -DENABLE_AZURE=ON)
ARG SHARKBITE_OPTIONS=""
ARG CMAKE_BUILD_TYPE=Release
ARG DOCKER_SKIP_TESTS=ON

# Install the system dependencies needed for a build
RUN apk --no-cache add gcc \
  g++ \
  make \
  bash \
  bison \
  flex \
  flex-dev \
  linux-headers \
  maven \
  openjdk8-jre-base \
  openjdk8 \
  autoconf \
  automake \
  libtool \
  curl-dev \
  cmake \
  git \
  patch \
  libpcap-dev \
  libpng-dev \
  libusb-dev \
  gpsd-dev \
  python3-dev \
  boost-dev \
  doxygen \
  ccache \
  lua-dev

ENV USER SHARKBITEcpp
ENV SHARKBITE_BASE_DIR /opt/SHARKBITE
ENV JAVA_HOME /usr/lib/jvm/default-jvm
ENV PATH ${PATH}:/usr/lib/jvm/default-jvm/bin
ENV SHARKBITE_HOME $SHARKBITE_BASE_DIR/nifi-SHARKBITE-cpp-${SHARKBITE_VERSION}
ENV SHARKBITE_VERSION ${SHARKBITE_VERSION}

# Setup SHARKBITEcpp user
RUN addgroup -g ${GID} ${USER} && adduser -u ${UID} -D -G ${USER} -g "" ${USER} && \
    install -d -o ${USER} -g ${USER} ${SHARKBITE_BASE_DIR}
COPY --chown=${USER}:${USER} . ${SHARKBITE_BASE_DIR}

USER ${USER}

RUN mkdir ${SHARKBITE_BASE_DIR}/build
WORKDIR ${SHARKBITE_BASE_DIR}/build
RUN export PATH=/usr/lib64/ccache/bin${PATH:+:${PATH}} && \
    export CCACHE_DIR=${SHARKBITE_BASE_DIR}/.ccache && \
    cmake -DSTATIC_BUILD= -DSKIP_TESTS=${DOCKER_SKIP_TESTS} ${SHARKBITE_OPTIONS} -DAWS_ENABLE_UNITY_BUILD=OFF -DCMAKE_BUILD_TYPE="${CMAKE_BUILD_TYPE}" .. && \
    make -j "$(nproc)" package && \
    tar -xzvf "${SHARKBITE_BASE_DIR}/build/nifi-SHARKBITE-cpp-${SHARKBITE_VERSION}.tar.gz" -C "${SHARKBITE_BASE_DIR}"

# Release image
FROM ${BASE_ALPINE_IMAGE} AS release
LABEL maintainer="Marc Parisi <phrocker@apache.org>"

ARG UID=1000
ARG GID=1000
ARG SHARKBITE_VERSION

ARG SHARKBITE_OPTIONS

# Add testing repo for rocksdb
RUN echo 'http://dl-cdn.alpinelinux.org/alpine/edge/testing' >> /etc/apk/repositories

ENV USER SHARKBITEcpp
ENV SHARKBITE_BASE_DIR /opt/SHARKBITE
ENV SHARKBITE_HOME ${SHARKBITE_BASE_DIR}/SHARKBITE-current
ENV SHARKBITE_VERSIONED_HOME ${SHARKBITE_BASE_DIR}/nifi-SHARKBITE-cpp-${SHARKBITE_VERSION}
ENV JAVA_HOME /usr/lib/jvm/default-jvm
ENV PATH ${PATH}:/usr/lib/jvm/default-jvm/bin

RUN addgroup -g ${GID} ${USER} && adduser -u ${UID} -D -G ${USER} -g "" ${USER} && \
    install -d -o ${USER} -g ${USER} ${SHARKBITE_BASE_DIR} && ln -s ${SHARKBITE_VERSIONED_HOME} ${SHARKBITE_HOME} && \
    apk add --no-cache libstdc++ tzdata alpine-conf && \
    if echo "$SHARKBITE_OPTIONS" | grep -q "ENABLE_GPS=ON"; then apk add --no-cache gpsd; fi && \
    if echo "$SHARKBITE_OPTIONS" | grep -q "ENABLE_JNI=ON"; then apk add --no-cache openjdk8-jre-base; fi && \
    if echo "$SHARKBITE_OPTIONS" | grep -q "ENABLE_PCAP=ON"; then apk add --no-cache libpcap; fi && \
    if echo "$SHARKBITE_OPTIONS" | grep -q "ENABLE_USB_CAMERA=ON"; then apk add --no-cache libpng libusb; fi && \
    if echo "$SHARKBITE_OPTIONS" | grep -q "ENABLE_OPENCV=ON" || echo "$SHARKBITE_OPTIONS" | grep -q "ENABLE_BUSTACHE=ON"; then apk add --no-cache boost; fi && \
    if echo "$SHARKBITE_OPTIONS" | grep -q "ENABLE_LUA_SCRIPTING=ON"; then apk add --no-cache lua; fi && \
    if echo "$SHARKBITE_OPTIONS" | grep -q "ENABLE_PYTHON_SCRIPTING=ON"; then apk add --no-cache python3; fi

# Copy built SHARKBITE distribution from builder
COPY --from=build --chown=${USER}:${USER} ${SHARKBITE_VERSIONED_HOME} ${SHARKBITE_HOME}
COPY --from=build --chown=${USER}:${USER} ${SHARKBITE_BASE_DIR}/docker/conf/SHARKBITE-log.properties ${SHARKBITE_HOME}/conf/SHARKBITE-log.properties
RUN setup-timezone -z UTC

USER ${USER}
WORKDIR ${SHARKBITE_HOME}

# Start SHARKBITE CPP in the foreground
CMD ["./bin/SHARKBITE.sh", "run"]

