cmake_minimum_required(VERSION 3.0)
project (Sharkbite)
set ( CMAKE_VERBOSE_MAKEFILE true )

include(CheckCXXCompilerFlag)

option(PYTHON_ITERATOR_SUPPORT "Builds python iterator support." OFF)
option(BUILD_JAR "Builds python iterator support. Deprecated by PYTHON_ITERATOR_SUPPORT" OFF)

CHECK_CXX_COMPILER_FLAG("-std=c++14" COMPILER_SUPPORTS_CXX11)
CHECK_CXX_COMPILER_FLAG("-std=c++0x" COMPILER_SUPPORTS_CXX0X)
if(COMPILER_SUPPORTS_CXX14)
    set(CMAKE_CXX_FLAGS "${CMAKE_CXX_FLAGS} -std=c++14")
elseif(COMPILER_SUPPORTS_CXX0X)
    set(CMAKE_CXX_FLAGS "${CMAKE_CXX_FLAGS} -std=c++0x")
else()
 message(STATUS "The compiler ${CMAKE_CXX_COMPILER} has no C++14 support. Please use a different C++ compiler.")
endif()


set (CMAKE_CXX_FLAGS "-std=c++14 -g -Wno-switch -Wall -fomit-frame-pointer -fno-stack-protector -Wno-unused-value -Wno-unused-function -Wno-reorder -Wno-overloaded-virtual -Wno-char-subscripts -Wno-unknown-pragmas ")
#  -fsingle-precision-constant 
option(test "Build all tests." OFF)
option(BUILD_PYTHON_TESTS "Build Python Integration." OFF)  
option(BUILD_TESTS "Build all tests." ON)
include(ExternalProject)
include_directories(${CMAKE_CURRENT_SOURCE_DIR}/include)

# Use ccache if present
find_program(CCACHE_FOUND ccache)
if(CCACHE_FOUND)
    set_property(GLOBAL PROPERTY RULE_LAUNCH_COMPILE ccache)
    set_property(GLOBAL PROPERTY RULE_LAUNCH_LINK ccache)
    message("-- Found ccache: ${CCACHE_FOUND}")
endif(CCACHE_FOUND)

if(NOT APPLE)
  if(NOT WIN32)
  	set(WHOLE_ARCHIVE "-Wl,--whole-archive")
    set(NO_WHOLE_ARCHIVE "-Wl,--no-whole-archive")
  endif()
endif()

set(CMAKE_MODULE_PATH ${PROJECT_SOURCE_DIR}/cmake/modules)
####

include(Compression)
include(DownloadProject)
include(LibreSSL)

use_bundled_zlib(${CMAKE_CURRENT_SOURCE_DIR} ${CMAKE_CURRENT_BINARY_DIR})
list(APPEND CMAKE_MODULE_PATH "${CMAKE_CURRENT_SOURCE_DIR}/cmake/override/")
find_package(ZLIB REQUIRED)

use_libre_ssl(${CMAKE_CURRENT_SOURCE_DIR} ${CMAKE_CURRENT_BINARY_DIR})
find_package (OpenSSL REQUIRED)
find_package(Boost 1.55 REQUIRED COMPONENTS system thread serialization)
include_directories(SYSTEM ${Boost_INCLUDE_DIR} )
################# THRIFT

	ExternalProject_Add(
	  LibThrift
	  GIT_REPOSITORY "https://github.com/apache/thrift.git"
	  GIT_TAG "v0.12.0"
	  BUILD_IN_SOURCE true
	  SOURCE_DIR "${CMAKE_CURRENT_BINARY_DIR}/dependencies/thrift-src"
	  	  CMAKE_ARGS "-DCMAKE_INSTALL_PREFIX=${CMAKE_CURRENT_BINARY_DIR}/thrift"
	  			 "-DCMAKE_POSITION_INDEPENDENT_CODE=ON"
	  			 "-DWITH_BOOST_STATIC=ON"
	  			 "-DBUILD_TESTING=OFF"
	  			 "-DCMAKE_MODULE_PATH=${CMAKE_MODULE_PATH}"
	  			 "-DWITH_JAVA=OFF"
	  			 "-DWITH_LIBEVENT=ON"
	  			 "-DWITH_PYTHON=OFF"
	  			 "-DLIBRESSL_BIN_DIR=${LIBRESSL_BIN_DIR}"
                		"-DLIBRESSL_SRC_DIR=${LIBRESSL_BIN_DIR}"
	        	        "-DBYPRODUCT_PREFIX=${BYPRODUCT_PREFIX}"
        	        	"-DBYPRODUCT_SUFFIX=${BYPRODUCT_SUFFIX}"
	                	"-DZLIB_BYPRODUCT_INCLUDE=${ZLIB_BYPRODUCT_INCLUDE}"
        		        "-DZLIB_BYPRODUCT=${ZLIB_BYPRODUCT}"
		                "-DZLIB_LIBRARY=${ZLIB_LIBRARY}"
                		"-DZLIB_LIBRARIES=${ZLIB_LIBRARIES}"
	  			 "-DOPENSSL_ROOT_DIR=${LIBRESSL_BIN_DIR}"
	  			 "-DWITH_HASKELL=OFF"
	  			 "-DWITH_SHARED_LIB=OFF"
	  			 "-DWITH_C_GLIB=OFF"
#CONFIGURE_COMMAND ./bootstrap.sh && ./configure --without-qt4  --with-openssl=${LIBRESSL_BIN_DIR} --without-c_glib  --without-csharp --without-java --without-erlang --without-nodejs --without-lua --without-python --without-perl --without-php --without-php_extension --without-ruby --without-haskell --without-go --without-d --prefix=${CMAKE_CURRENT_BINARY_DIR}/dependencies/thrift
	  UPDATE_COMMAND ""
	  )

	add_dependencies(LibThrift zlib-external)	
	add_dependencies(LibThrift libressl-portable)
	add_library(thrift STATIC IMPORTED)
	set_target_properties(thrift PROPERTIES IMPORTED_LOCATION "${CMAKE_CURRENT_BINARY_DIR}/thrift/lib/libthrift.a")
	add_dependencies(thrift LibThrift)
	set(THRIFT_INCLUDE_DIR "${CMAKE_CURRENT_BINARY_DIR}/thrift/include" CACHE STRING "" FORCE)
	set(THRIFT_LIBS  "${CMAKE_CURRENT_BINARY_DIR}/thrift/lib/libthrift.a" CACHE STRING "" FORCE)
	include_directories(${THRIFT_INCLUDE_DIR})
	set(THRIFT_FOUND TRUE CACHE STRING "" FORCE)

#################### ZOOKEEPER

	ExternalProject_Add(
	  ZooKeeper
	  GIT_REPOSITORY "https://github.com/phrocker/zookeeper.git"
	  GIT_TAG sharkbite-3.4.8-d
	  GIT_PROGRESS ON
	  BUILD_IN_SOURCE true
	  SOURCE_DIR "${CMAKE_CURRENT_BINARY_DIR}/dependencies/zookeeper-src"
	  #CMAKE_ARGS "-DCMAKE_INSTALL_PREFIX=${CMAKE_CURRENT_BINARY_DIR}/zookeeper"
	  CONFIGURE_COMMAND autoreconf -if && ./configure --prefix=${CMAKE_CURRENT_BINARY_DIR}/dependencies/zookeeper
	  UPDATE_COMMAND ""
	)
	
	add_library(zookeeper STATIC IMPORTED)
	set_target_properties(zookeeper PROPERTIES IMPORTED_LOCATION "${CMAKE_CURRENT_BINARY_DIR}/dependencies/zookeeper/lib/libzookeeper_mt.a")
	add_dependencies(zookeeper ZooKeeper)
	
	set(ZOOKEEPER_INCLUDE_DIRS "${CMAKE_CURRENT_BINARY_DIR}/dependencies/zookeeper/include" CACHE STRING "" FORCE)
	set(ZOOKEEPER_LIBRARIES  "${CMAKE_CURRENT_BINARY_DIR}/dependencies/zookeeper/lib/libzookeeper_mt.a" CACHE STRING "" FORCE)
	#set(ZOOKEEPER_SHARED_LIBRARIES  "${CMAKE_CURRENT_BINARY_DIR}/dependencies/zookeeper/lib/libzookeeper_mt.so" CACHE STRING "" FORCE)
	include_directories(${ZOOKEEPER_INCLUDE_DIRS})
	set(ZOOKEEPER_FOUND TRUE CACHE STRING "" FORCE)

######################  PROTOBUF


	ExternalProject_Add(
	  Protobuf
	  GIT_REPOSITORY "https://github.com/phrocker/protobuf.git"
	  GIT_TAG v3.1.0
	  BUILD_IN_SOURCE true
	  SOURCE_DIR "${CMAKE_CURRENT_BINARY_DIR}/dependencies/protobuf-src"
	  #CMAKE_ARGS "-DCMAKE_INSTALL_PREFIX=${CMAKE_CURRENT_BINARY_DIR}/protobuf"
	  CONFIGURE_COMMAND ./autogen.sh && ./configure --prefix=${CMAKE_CURRENT_BINARY_DIR}/dependencies/protobuf
	  UPDATE_COMMAND ""
	  )
	
	add_library(protobuf STATIC IMPORTED)
	set_target_properties(protobuf PROPERTIES IMPORTED_LOCATION "${CMAKE_CURRENT_BINARY_DIR}/dependencies/protobuf/lib/libprotobuf.a" )
	set(PROTOBUF_LIBS "${CMAKE_CURRENT_BINARY_DIR}/dependencies/protobuf/lib/libprotobuf.a" CACHE STRING "" FORCE)
	set(PROTOBUF_INCLUDE_DIRS "${CMAKE_CURRENT_BINARY_DIR}/dependencies/protobuf/include" CACHE STRING "" FORCE)
	add_dependencies(protobuf Protobuf)
	set(PROTOBUF_FOUND TRUE CACHE STRING "" FORCE)


find_package(Python COMPONENTS Interpreter Development)

download_project(PROJ pybind11 
				 GIT_REPOSITORY "https://github.com/pybind/pybind11.git" 
				 GIT_TAG "7ec2ddfc95f65d1e986d359466a6c254aa514ef3" )

set(PYBIND11_CPP_STANDARD -std=c++14)
add_subdirectory(${pybind11_SOURCE_DIR})
include_directories("${pybind11_SOURCE_DIR}/include/")
#find_package(GTest REQUIRED)
 

find_package (Threads)

include_directories(SYSTEM ${THRIFT_INCLUDE_DIR}/thrift )
include_directories(SYSTEM ${ZOOKEEPER_INCLUDE_DIRS})
include_directories(SYSTEM ${ZLIB_INCLUDE_DIRS})
include_directories(SYSTEM ${GTEST_INCLUDE_DIRS})
message(STATUS "GTest include dir :  ${GTEST_INCLUDE_DIRS}")
message(STATUS "GTest lib : ${GTEST_LIBRARIES}")
message(STATUS "Thrift include dir: ${THRIFT_INCLUDE_DIR}/thrift")
message(STATUS "Thrift library path: ${THRIFT_LIBS}")
message(STATUS "Thrift static library: ${THRIFT_STATIC_LIB}")
message(STATUS "ZK Include: ${ZOOKEEPER_INCLUDE_DIRS}")
message(STATUS "ZK Lib: ${ZOOKEEPER_LIBRARIES}")


#However, the file(GLOB...) allows for wildcard additions:
file(GLOB_RECURSE EXCEPTION_SOURCES "src/data/exceptions/*cpp")
file(GLOB_RECURSE CONSTRUCT_SOURCES "src/data/constructs/*cpp")
file(GLOB_RECURSE STREAMING_SOURCES "src/data/streaming/*cpp")
file(GLOB_RECURSE ZK_SOURCES "src/data/zookeeper/*cpp")
file(GLOB_RECURSE CLIENT_SOURCES "src/data/client/*cpp")
file(GLOB_RECURSE CAPI_SOURCES "src/extern/*cpp")
file(GLOB_RECURSE UTIL_SOURCES "src/utils/*cpp")
file(GLOB_RECURSE LOGGING_SOURCES "src/logging/*cpp")
file(GLOB_RECURSE THRIFT_SOURCES "src/data/extern/thrift/*cpp")
file(GLOB_RECURSE THRIFTV2_SOURCES "src/data/extern/thriftv2/*cpp")
file(GLOB_RECURSE CWRAPPER_SOURCES "src/extern/*cpp")
file(GLOB_RECURSE CWRAPPER_SOURCES "src/extern/*cpp")
file(GLOB_RECURSE PYBIND_SOURCES "src/python/*cpp")
file(GLOB_RECURSE INTERCONNECT_SOURCES "src/interconnect/*cpp")
file(GLOB_RECURSE SCANNER_SOURCES "src/scanner/*cpp")
file(GLOB_RECURSE WRITER_SOURCES "src/writer/*cpp")
if (BUILD_JAR OR PYTHON_ITERATOR_SUPPORT)
	find_package(JNI REQUIRED)
	find_package(Java REQUIRED)
	find_package(Maven REQUIRED)
	
	include_directories(${JNI_INCLUDE_DIRS})
	
	include_directories(${JNI_INCLUDE_DIRS})
	file(GLOB_RECURSE JNI_SOURCES "src/jni/*cpp")
else()
set(JNI_SOURCES "")	
	
endif(BUILD_JAR OR PYTHON_ITERATOR_SUPPORT)

include_directories("thirdparty/spdlog-20170710/include/")
include_directories("thirdparty/gzip-hpp/")


add_library(sharkbite STATIC  ${UTIL_SOURCES} ${LOGGING_SOURCES} ${ZK_SOURCES} ${CONSTRUCT_SOURCES} ${STREAMING_SOURCES} ${CLIENT_SOURCES} ${EXCEPTION_SOURCES} ${THRIFT_SOURCES} ${THRIFTV2_SOURCES} ${INTERCONNECT_SOURCES} ${SCANNER_SOURCES} ${WRITER_SOURCES} ${CWRAPPER_SOURCES} )

add_library(sharkbite-shared SHARED ${UTIL_SOURCES} ${LOGGING_SOURCES} ${ZK_SOURCES} ${CONSTRUCT_SOURCES} ${STREAMING_SOURCES} ${CLIENT_SOURCES} ${EXCEPTION_SOURCES} ${THRIFT_SOURCES} ${THRIFTV2_SOURCES} ${INTERCONNECT_SOURCES} ${SCANNER_SOURCES} ${WRITER_SOURCES} ${CWRAPPER_SOURCES} )





pybind11_add_module(pysharkbite ${PYBIND_SOURCES} ${LOGGING_SOURCES} ${UTIL_SOURCES} ${ZK_SOURCES} ${CONSTRUCT_SOURCES} ${STREAMING_SOURCES} ${CLIENT_SOURCES} ${EXCEPTION_SOURCES} ${THRIFT_SOURCES} ${THRIFTV2_SOURCES} ${INTERCONNECT_SOURCES} ${SCANNER_SOURCES} ${WRITER_SOURCES} ${CWRAPPER_SOURCES} )
if (BUILD_JAR OR PYTHON_ITERATOR_SUPPORT)
	pybind11_add_module(pysharkbite-iterators ${LOGGING_SOURCES} ${UTIL_SOURCES} ${ZK_SOURCES} ${CONSTRUCT_SOURCES} ${STREAMING_SOURCES} ${CLIENT_SOURCES} ${EXCEPTION_SOURCES} ${THRIFT_SOURCES} ${THRIFTV2_SOURCES} ${INTERCONNECT_SOURCES} ${SCANNER_SOURCES} ${WRITER_SOURCES} ${CWRAPPER_SOURCES} ${JNI_SOURCES})
endif()


add_dependencies(sharkbite Protobuf)
add_dependencies(pysharkbite Protobuf)
add_dependencies(sharkbite-shared Protobuf)


add_dependencies(sharkbite ZooKeeper)
add_dependencies(pysharkbite ZooKeeper)
add_dependencies(sharkbite-shared ZooKeeper)

add_dependencies(sharkbite LibThrift)
add_dependencies(pysharkbite LibThrift)
add_dependencies(sharkbite-shared LibThrift)

target_link_libraries (sharkbite ${CMAKE_THREAD_LIBS_INIT} ${ZOOKEEPER_LIBRARIES}   ${OPENSSL_LIBRARIES} ${THRIFT_LIBS} )


target_link_libraries(sharkbite 
  ${WHOLE_ARCHIVE}
  ${ZLIB_LIBRARY}
  ${NO_WHOLE_ARCHIVE}
)


target_link_libraries (pysharkbite PRIVATE ${ZOOKEEPER_LIBRARIES}   ${OPENSSL_LIBRARIES} ${THRIFT_LIBS})

if (BUILD_JAR OR PYTHON_ITERATOR_SUPPORT)
	target_link_libraries (pysharkbite-iterators PRIVATE ${ZOOKEEPER_LIBRARIES}   ${OPENSSL_LIBRARIES} ${THRIFT_LIBS})
	target_link_libraries (pysharkbite-iterators PRIVATE  pybind11::embed ${JAVA_JVM_LIBRARY})	
endif(BUILD_JAR OR PYTHON_ITERATOR_SUPPORT)

if (NOT APPLE)
	target_link_libraries (pysharkbite PRIVATE -static-libgcc -static-libstdc++  Threads::Threads )
	if (BUILD_JAR OR PYTHON_ITERATOR_SUPPORT)
		target_link_libraries (pysharkbite-iterators PRIVATE -static-libgcc -static-libstdc++  Threads::Threads )
	endif(BUILD_JAR OR PYTHON_ITERATOR_SUPPORT)
endif()
target_compile_definitions( pysharkbite PRIVATE -DPYTHON=1)
target_compile_definitions( pysharkbite PRIVATE -DPYTHON=1)
target_link_libraries(pysharkbite  PRIVATE
  ${WHOLE_ARCHIVE}
  ${ZLIB_LIBRARY}
  ${NO_WHOLE_ARCHIVE}
)


target_compile_options(pysharkbite INTERFACE -fvisibility=hidden)

if (BUILD_JAR OR PYTHON_ITERATOR_SUPPORT)
target_link_libraries(pysharkbite-iterators  PRIVATE
  ${WHOLE_ARCHIVE}
  ${ZLIB_LIBRARY}
  ${NO_WHOLE_ARCHIVE}
)
target_compile_options(pysharkbite-iterators INTERFACE -fvisibility=hidden)
endif()


target_link_libraries (sharkbite-shared ${CMAKE_THREAD_LIBS_INIT} ${OPENSSL_LIBRARIES})

target_link_libraries (sharkbite-shared ${ZOOKEEPER_LIBRARIES})

target_link_libraries(sharkbite-shared ${WHOLE_ARCHIVE} ${ZLIB_LIBRARY} ${NO_WHOLE_ARCHIVE}
)


target_link_libraries (sharkbite-shared ${THRIFT_LIBS})


add_library(capi SHARED ${CAPI_SOURCES})
add_dependencies(capi sharkbite-shared)
target_link_libraries(capi PRIVATE sharkbite-shared)

#ClientExample
add_executable(ClientExample "src/examples/ClientExample.cpp")
target_link_libraries (ClientExample ${CMAKE_THREAD_LIBS_INIT})
#target_link_libraries (ClientExample ${WHOLE_ARCHIVE} ${ZLIB_LIBRARY} ${NO_WHOLE_ARCHIVE})
target_link_libraries( ClientExample )
target_link_libraries( ClientExample ${ZOOKEEPER_LIBRARIES} ${OPENSSL_LIBRARIES})
target_link_libraries( ClientExample ${THRIFT_LIBS} sharkbite)

if (BUILD_TESTS)
enable_testing()
add_executable(InsertTest "src/examples/InsertTest.cpp")
target_link_libraries (InsertTest ${CMAKE_THREAD_LIBS_INIT})
#target_link_libraries (InsertTest ${WHOLE_ARCHIVE} ${ZLIB_LIBRARY} ${NO_WHOLE_ARCHIVE})
target_link_libraries( InsertTest )
target_link_libraries( InsertTest ${ZOOKEEPER_LIBRARIES} )
target_link_libraries( InsertTest ${THRIFT_LIBS} ${OPENSSL_LIBRARIES})
target_link_libraries( InsertTest sharkbite)



add_executable(ContinuousIngest "src/examples/ContinuousIngest.cpp")
target_link_libraries (ContinuousIngest ${CMAKE_THREAD_LIBS_INIT})
#target_link_libraries (ContinuousIngest ${WHOLE_ARCHIVE} ${ZLIB_LIBRARY} ${NO_WHOLE_ARCHIVE})
target_link_libraries( ContinuousIngest )
target_link_libraries( ContinuousIngest ${ZOOKEEPER_LIBRARIES} ${OPENSSL_LIBRARIES})
target_link_libraries( ContinuousIngest ${THRIFT_LIBS})
target_link_libraries( ContinuousIngest sharkbite)

add_executable(TableOps "src/examples/TableOps.cpp")
target_link_libraries (TableOps ${CMAKE_THREAD_LIBS_INIT})
#target_link_libraries (TableOps ${WHOLE_ARCHIVE} ${ZLIB_LIBRARY} ${NO_WHOLE_ARCHIVE})
target_link_libraries( TableOps )
target_link_libraries( TableOps ${ZOOKEEPER_LIBRARIES} )
target_link_libraries( TableOps ${THRIFT_LIBS} ${OPENSSL_LIBRARIES})
target_link_libraries( TableOps sharkbite)

add_executable(RfileReadExample "src/examples/RfileReadExample.cpp")
target_link_libraries (RfileReadExample ${CMAKE_THREAD_LIBS_INIT})
#target_link_libraries (RfileReadExample ${WHOLE_ARCHIVE} ${ZLIB_LIBRARY} ${NO_WHOLE_ARCHIVE})
target_link_libraries( RfileReadExample )
target_link_libraries( RfileReadExample ${ZOOKEEPER_LIBRARIES} ${OPENSSL_LIBRARIES} )
target_link_libraries( RfileReadExample ${THRIFT_LIBS})
target_link_libraries( RfileReadExample sharkbite)

add_executable(PrintInfo "src/examples/PrintInfo.cpp")
target_link_libraries (PrintInfo ${CMAKE_THREAD_LIBS_INIT})
#target_link_libraries (PrintInfo ${WHOLE_ARCHIVE} ${ZLIB_LIBRARY} ${NO_WHOLE_ARCHIVE})
target_link_libraries( PrintInfo )
target_link_libraries( PrintInfo ${ZOOKEEPER_LIBRARIES} ${OPENSSL_LIBRARIES} )
target_link_libraries( PrintInfo ${THRIFT_LIBS})
target_link_libraries( PrintInfo sharkbite)


ADD_CUSTOM_TARGET(debian-package
	COMMAND dpkg-buildpackage -us -uc -b
	WORKING_DIRECTORY ${CMAKE_SOURCE_DIR}
	COMMENT "Create debian package..."
)




  file(GLOB_RECURSE RFILE_TEST "test/constructs/rfile_test.cpp")
  add_executable(testrfile ${RFILE_TEST})
  target_link_libraries(testrfile  ${CMAKE_THREAD_LIBS_INIT} sharkbite)

  
  add_test(NAME testrfile
	   COMMAND testrfile)
	   
  file(GLOB_RECURSE TEST_CONSTRUCTS "test/constructs/TestConstructs.cpp")
  add_executable(testconstructs ${TEST_CONSTRUCTS})
  target_link_libraries(testconstructs  ${CMAKE_THREAD_LIBS_INIT} sharkbite)

  
  add_test(NAME testconstructs
	   COMMAND testconstructs)
	   
  file(GLOB_RECURSE TEST_STREAMS "test/constructs/TestStreams.cpp")
  add_executable(teststreams ${TEST_STREAMS})
  target_link_libraries(teststreams  ${CMAKE_THREAD_LIBS_INIT} sharkbite)
  
  add_test(NAME teststreams
	   COMMAND teststreams)
	   
 
    file(GLOB_RECURSE TEST_ZK_INSTANCE "test/zookeeper/testInstance.cpp")
  add_executable(testzkInstance ${TEST_ZK_INSTANCE})
  target_link_libraries (testzkInstance ${CMAKE_THREAD_LIBS_INIT})
  target_link_libraries( testzkInstance )
  target_link_libraries( testzkInstance ${ZOOKEEPER_LIBRARIES} )
  target_link_libraries( testzkInstance ${OPENSSL_LIBRARIES} ${THRIFT_LIBS} sharkbite)

  target_link_libraries(testzkInstance sharkbite)
  
      file(GLOB_RECURSE VANDV "test/vandv/IntegrationTest.cpp")
  add_executable(integrationTest ${VANDV})
  target_link_libraries (integrationTest ${CMAKE_THREAD_LIBS_INIT})
  target_link_libraries( integrationTest )
  target_link_libraries( integrationTest ${ZOOKEEPER_LIBRARIES} )
  target_link_libraries( integrationTest  ${THRIFT_LIBS} ${OPENSSL_LIBRARIES})
  target_link_libraries(integrationTest   sharkbite)


if (BUILD_PYTHON_TESTS)

	set(PYTHON_TEST_SOURCE "${CMAKE_SOURCE_DIR}/test/python")
	set(PYTHON_TEST_DEST "${CMAKE_CURRENT_BINARY_DIR}/test/" )
		
	file(COPY ${PYTHON_TEST_SOURCE} DESTINATION ${PYTHON_TEST_DEST})
	
	add_custom_command(TARGET pysharkbite POST_BUILD COMMAND ${CMAKE_COMMAND} -E copy $<TARGET_FILE:pysharkbite> ${PYTHON_TEST_DEST}/python/)
	
	add_subdirectory(test/services)
	
	include_directories(${JNI_INCLUDE_DIRS})
		
	set(MAC19X "${CMAKE_SOURCE_DIR}/test/19x/")
	set(MAX19XBIN "${CMAKE_CURRENT_BINARY_DIR}/test" )
		
	file(COPY ${MAC19X} DESTINATION ${MAX19XBIN})
	find_package(Java)
	find_package(Maven )
	
	if (MAVEN_FOUND)
		add_custom_command(TARGET pysharkbite POST_BUILD COMMAND "${MAVEN_EXECUTABLE}" "-q" "package" "-Denforcer.skip=true" WORKING_DIRECTORY ${MAC19X}/standalone-mac)
	
	
	#mrthesegfault /usr/bin/python3.7 ~/dev/sharkbite/test/python/ ~/dev/sharkbite/build/pysharkbite.cpython-37m-x86_64-linux-gnu.so 
	
	cmake_host_system_information(RESULT _host_name QUERY HOSTNAME)
	
	set(SHARED_OBJ ${PYTHON_TEST_DEST}/python/$<TARGET_FILE_NAME:pysharkbite>)
	add_test(
	    NAME RunPythonTests 
	    COMMAND ${Java_JAVA_EXECUTABLE} -jar ${MAX19XBIN}/standalone-mac/target/standalone-mac-shaded-193.3.jar ${_host_name} ${Python_EXECUTABLE} ${PYTHON_TEST_DEST}/python ${SHARED_OBJ})
	
	
	else()
	message(STATUS "Maven not found, so not building java tests")
	
	
	
	endif(MAVEN_FOUND)

endif(BUILD_PYTHON_TESTS)

endif(BUILD_TESTS)

add_subdirectory(include/interconnect/securityOps)

### JAR building
if (BUILD_JAR OR PYTHON_ITERATOR_SUPPORT)
	message(STATUS "MAVEN: ${MAVEN_EXECUTABLE}")
	include_directories(${JNI_INCLUDE_DIRS})
	
	set(JNI_FRAMEWORK_JAR_SOURCE "${CMAKE_SOURCE_DIR}/native-iterators-jni")
	set(JNI_FRAMEWORK_JAR_BIN "${CMAKE_CURRENT_BINARY_DIR}/" )
	
	file(COPY ${JNI_FRAMEWORK_JAR_SOURCE} DESTINATION ${JNI_FRAMEWORK_JAR_BIN})
	
	add_custom_command(TARGET pysharkbite-iterators POST_BUILD COMMAND ${CMAKE_COMMAND} -E copy $<TARGET_FILE:pysharkbite-iterators> ${JNI_FRAMEWORK_JAR_BIN}/native-iterators-jni/src/main/resources/)
	
	add_custom_command(TARGET pysharkbite-iterators POST_BUILD COMMAND "${MAVEN_EXECUTABLE}" "-q" "package" "-Denforcer.skip=true" WORKING_DIRECTORY ${JNI_FRAMEWORK_JAR_BIN}/native-iterators-jni)
	
endif(BUILD_JAR OR PYTHON_ITERATOR_SUPPORT)

###

FIND_PACKAGE(Doxygen)
if(DOXYGEN_FOUND)
 
MESSAGE("Creating API Documentation")
SET(DOXYGEN_INPUT "../docs/Doxyfile")
SET(DOXYGEN_OUTPUT "../docs")

ADD_CUSTOM_COMMAND(
  OUTPUT ${DOXYGEN_OUTPUT}
  COMMAND ${CMAKE_COMMAND} -E echo_append "Building API Documentation..."
  COMMAND ${DOXYGEN_EXECUTABLE} ${DOXYGEN_INPUT}
  COMMAND ${CMAKE_COMMAND} -E echo "Done."
  WORKING_DIRECTORY ${CMAKE_CURRENT_SOURCE_DIR}
  DEPENDS ${DOXYGEN_INPUT}
  )

ADD_CUSTOM_TARGET(docs ${DOXYGEN_EXECUTABLE} ${DOXYGEN_INPUT})
endif(DOXYGEN_FOUND)	
# Install library headers

include(GNUInstallDirs)
file(GLOB RECURSE HEADERS include/*.h)
#install(FILES ${HEADERS} DESTINATION include/${PROJECT_NAME})
message("Received ${CMAKE_INSTALL_INCLUDEDIR}")
set_target_properties(sharkbite PROPERTIES PUBLIC_HEADER "${HEADERS}")
install(TARGETS sharkbite
    ARCHIVE DESTINATION ${CMAKE_INSTALL_LIBDIR}
    LIBRARY DESTINATION ${CMAKE_INSTALL_LIBDIR}
    PUBLIC_HEADER DESTINATION include/
    COMPONENT LIBRARY ) 
INSTALL (
    DIRECTORY ${CMAKE_SOURCE_DIR}/include/
    DESTINATION include/
    FILES_MATCHING PATTERN "*.h*")
#install(DIRECTORY ${PROJECT_SOURCE_DIR}/include
 #       DESTINATION )
