# Copyright 2012 Cloudera Inc.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
# http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# - Find libhdfs3 ( Pivotal's Native c/c++ hdfs client
# This module defines
# HDFS3_INCLUDE_DIR
# HDFS3_LIB_DIR

find_path(HDFS3_INCLUDE_DIR hdfs/hdfs.h HINTS
  /usr/local/include/
  /opt/local/include/
)
message(STATUS "HDFS3_INCLUDE_DIR: ${HDFS3_INCLUDE_DIR}")

set(HDFS3_LIB_PATHS
  /usr/local/lib
  /opt/local/lib)

find_path(HDFS3_STATIC_LIB_PATH libhdfs3.a PATHS ${HDFS3_LIB_PATHS})

message (STATUS "HDFS3_STATIC_LIB_PATH: ${HDFS3_STATIC_LIB_PATH}")

find_library(HDFS3_LIB NAMES libhdfs3 hdfs3 HINTS ${HDFS3_LIB_PATHS})

message (STATUS "HDFS3_LIB: ${HDFS3_LIB}")

if (HDFS3_LIB)
  set(HDFS3_FOUND TRUE)
  set(HDFS3_LIBS ${HDFS3_LIB})
  set(HDFS3_STATIC_LIB ${HDFS3_STATIC_LIB_PATH}/libhdfs3.a)
else ()
  set(HDFS3_FOUND FALSE)
endif ()


mark_as_advanced(
  HDFS3_LIB
  HDFS3_COMPILER
  HDFS3_INCLUDE_DIR
)
