# Licensed to the Apache Software Foundation (ASF) under one
# or more contributor license agreements.  See the NOTICE file
# distributed with this work for additional information
# regarding copyright ownership.  The ASF licenses this file
# to you under the Apache License, Version 2.0 (the
# "License"); you may not use this file except in compliance
# with the License.  You may obtain a copy of the License at
#
#   http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing,
# software distributed under the License is distributed on an
# "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
# KIND, either express or implied.  See the License for the
# specific language governing permissions and limitations
# under the License.

function(use_libdeflate SOURCE_DIR BINARY_DIR)
 message(STATUS "Downloading and building libdeflate")
if (WIN32)
 string(TOLOWER "${CMAKE_BUILD_TYPE}" build_type)
 if (build_type MATCHES relwithdebinfo OR build_type MATCHES release)
 set(BYPRODUCT "thirdparty/libdeflate-src/libdeflate.lib")
 else()
 set(BYPRODUCT "thirdparty/libdeflate-src/libdeflate.lib")
 endif()
 else()
 set(BYPRODUCT "thirdparty/libdeflate-src/libdeflate.a")
  endif()
  ExternalProject_Add(
    libdeflate-external
    BUILD_IN_SOURCE true
    GIT_REPOSITORY "https://github.com/ebiggers/libdeflate.git"
    GIT_TAG "v1.7"  # zlib-ng
    SOURCE_DIR "${BINARY_DIR}/thirdparty/libdeflate-src"
    BUILD_COMMAND make
    CMAKE_COMMAND ""
    UPDATE_COMMAND ""
    BUILD_BYPRODUCTS ${BYPRODUCT}
    INSTALL_COMMAND ${CMAKE_COMMAND}  -E echo "Skipping install step."
    CONFIGURE_COMMAND ""
    EXCLUDE_FROM_ALL TRUE
      "${BUILD_ARGS}"
  )


  add_library(zlibdef STATIC IMPORTED)
  set_target_properties(zlibdef PROPERTIES IMPORTED_LOCATION "${BINARY_DIR}/${BYPRODUCT}")

  set(LIBDEFLATE_BYPRODUCT "${BINARY_DIR}/${BYPRODUCT}" CACHE STRING "" FORCE)
  set(LIBDEFLATE_BYPRODUCT_INCLUDE "${BINARY_DIR}/thirdparty/libdeflate-src/" CACHE STRING "" FORCE)
  set(LIBDEFLATE_BIN_DIR "${BINARY_DIR}/thirdparty/libdeflate-src/" CACHE STRING "" FORCE)

  add_dependencies(zlibdef libdeflate-external)
  set(LIBDEFLATE_FOUND "YES" CACHE STRING "" FORCE)
  set(LIBDEFLATE_INCLUDE_DIR "${LIBDEFLATE_BYPRODUCT_INCLUDE}" CACHE STRING "" FORCE)
  set(LIBDEFLATE_INCLUDE_DIRS "${LIBDEFLATE_BYPRODUCT_INCLUDE}" CACHE STRING "" FORCE)

  set(LIBDEFLATE_LIBRARY "${BINARY_DIR}/${BYPRODUCT}" CACHE STRING "" FORCE)
  set(LIBDEFLATE_LIBRARIES "${LIBDEFLATE_LIBRARY}" CACHE STRING "" FORCE)
  set(LIBDEFLATE_LIBRARY_RELEASE "${LIBDEFLATE_LIBRARY}" CACHE STRING "" FORCE)
  set(LIBDEFLATE_LIBRARY_DEBUG "${LIBDEFLATE_LIBRARY}" CACHE STRING "" FORCE)  
  
endfunction(use_libdeflate)