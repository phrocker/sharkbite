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

## adapted from another open source project I support that is Apache Licensed

function(download_lib_xml SOURCE_DIR BINARY_DIR)
	message("Downloading LibXML2")
	
	set(BYPRODUCT_PREFIX "lib" CACHE STRING "" FORCE)
	
	set(BYPRODUCT_SUFFIX ".a" CACHE STRING "" FORCE)
	
	set(BUILD_ARGS "")

	set(BYPRODUCT "${BINARY_DIR}/dependencies/libxml2-src/.libs/${BYPRODUCT_PREFIX}xml2${BYPRODUCT_SUFFIX}")
	ExternalProject_Add(
	libxml2-portable
	GIT_REPOSITORY "https://github.com/GNOME/libxml2.git"
	GIT_TAG "v2.9.10"
	SOURCE_DIR "${BINARY_DIR}/dependencies/libxml2-src"
	BUILD_IN_SOURCE TRUE
	CMAKE_ARGS ${PASSTHROUGH_CMAKE_ARGS}
	            BUILD_COMMAND make
                CMAKE_COMMAND ""
                UPDATE_COMMAND ""
                BUILD_BYPRODUCTS ${BYPRODUCT}
                INSTALL_COMMAND ${CMAKE_COMMAND}  -E echo "Skipping install step."
                CONFIGURE_COMMAND ./autogen.sh --with-lzma=no --with-zlib=no && ./configure
		  			"CFLAGS= -fPIC"
		  			"CXXFLAGS=${CMAKE_CXX_FLAGS} -fPIC"
                 EXCLUDE_FROM_ALL TRUE
				"${BUILD_ARGS}"
	)

	
	set(LIBXML2_SRC_DIR "${BINARY_DIR}/dependencies/libxml2-src/" CACHE STRING "" FORCE)
	set(LIBXML2_BIN_DIR "${BINARY_DIR}/dependencies/libxml2-src/" CACHE STRING "" FORCE)

	set(LibXml2_FOUND "YES" CACHE STRING "" FORCE)
	set(LIBXML2_INCLUDE_DIR "${LIBXML2_BIN_DIR}/include" CACHE STRING "" FORCE)
	set(LIBXML2_INCLUDE_DIRS "${LIBXML2_BIN_DIR}/include" CACHE STRING "" FORCE)
	set(LIBXML2_LIBRARIES "${BYPRODUCT}" CACHE STRING "" FORCE)
	set(LIBXML2_LIBRARY "${BYPRODUCT}" CACHE STRING "" FORCE)
	
endfunction(download_lib_xml) 
