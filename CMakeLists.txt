set(CMAKE_AUTOMOC ON)
cmake_minimum_required(VERSION 3.0)
project (Sharkbite)
set ( CMAKE_VERBOSE_MAKEFILE true )

include(CheckCXXCompilerFlag)

CHECK_CXX_COMPILER_FLAG("-std=c++11" COMPILER_SUPPORTS_CXX11)
CHECK_CXX_COMPILER_FLAG("-std=c++0x" COMPILER_SUPPORTS_CXX0X)
if(COMPILER_SUPPORTS_CXX11)
    set(CMAKE_CXX_FLAGS "${CMAKE_CXX_FLAGS} -std=c++11")
elseif(COMPILER_SUPPORTS_CXX0X)
    set(CMAKE_CXX_FLAGS "${CMAKE_CXX_FLAGS} -std=c++0x")
else()
 message(STATUS "The compiler ${CMAKE_CXX_COMPILER} has no C++11 support. Please use a different C++ compiler.")
endif()

set (CMAKE_CXX_FLAGS "-std=c++11 -g -Wall -fomit-frame-pointer -fno-stack-protector -Wno-unused-value -Wno-unused-function -Wno-reorder -Wno-overloaded-virtual -Wno-char-subscripts -Wno-tautological-constant-out-of-range-compare -Wno-unknown-pragmas ")
#  -fsingle-precision-constant 
option(test "Build all tests." OFF) 
include(ExternalProject)
include_directories(${CMAKE_CURRENT_SOURCE_DIR}/include)

# Use ccache if present
find_program(CCACHE_FOUND ccache)
if(CCACHE_FOUND)
    set_property(GLOBAL PROPERTY RULE_LAUNCH_COMPILE ccache)
    set_property(GLOBAL PROPERTY RULE_LAUNCH_LINK ccache)
    message("-- Found ccache: ${CCACHE_FOUND}")
endif(CCACHE_FOUND)


set(CMAKE_MODULE_PATH ${PROJECT_SOURCE_DIR}/cmake/modules)
####

include(LibreSSL)
use_libre_ssl(${CMAKE_CURRENT_SOURCE_DIR} ${CMAKE_CURRENT_BINARY_DIR})
list(APPEND CMAKE_MODULE_PATH "${CMAKE_CURRENT_SOURCE_DIR}/cmake/ssl")
find_package (OpenSSL REQUIRED)

################# THRIFT
#if (NOT THRIFT_FOUND)
	ExternalProject_Add(
	  LibThrift
	  GIT_REPOSITORY "https://github.com/phrocker/thrift.git"
	  GIT_TAG 8c4b75db00aa8f7f5fca44d14296a45030683dd8
	  BUILD_IN_SOURCE true
	  SOURCE_DIR "${CMAKE_CURRENT_BINARY_DIR}/dependencies/thrift-src"
	  CMAKE_ARGS "-DCMAKE_INSTALL_PREFIX=${CMAKE_CURRENT_BINARY_DIR}/thrift"
	  CONFIGURE_COMMAND ./bootstrap.sh && ./configure --without-qt4  --with-openssl=${LIBRESSL_BIN_DIR} --without-c_glib  --without-csharp --without-java --without-erlang --without-nodejs --without-lua --without-python --without-perl --without-php --without-php_extension --without-ruby --without-haskell --without-go --without-d --prefix=${CMAKE_CURRENT_BINARY_DIR}/dependencies/thrift
	  UPDATE_COMMAND ""
	  )
	
	add_dependencies(LibThrift libressl-portable)
	add_library(thrift STATIC IMPORTED)
	set_target_properties(thrift PROPERTIES IMPORTED_LOCATION "${CMAKE_CURRENT_BINARY_DIR}/dependencies/thrift/lib/libthrift.a")
	add_dependencies(thrift LibThrift)
	set(THRIFT_INCLUDE_DIR "${CMAKE_CURRENT_BINARY_DIR}/dependencies/thrift/include" CACHE STRING "" FORCE)
	set(THRIFT_LIBS  "${CMAKE_CURRENT_BINARY_DIR}/dependencies/thrift/lib/libthrift.a" CACHE STRING "" FORCE)
	set(THRIFT_SHARED_LIBS  "${CMAKE_CURRENT_BINARY_DIR}/dependencies/thrift/lib/libthrift.so" CACHE STRING "" FORCE)
	include_directories(${THRIFT_INCLUDE_DIR})
	set(THRIFT_FOUND TRUE CACHE STRING "" FORCE)
#else()#
	#include_directories(${THRIFT_INCLUDE_DIR})
#endif()
#################### ZOOKEEPER
##if (NOT ZOOKEEPER_FOUND)
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
	set(ZOOKEEPER_SHARED_LIBRARIES  "${CMAKE_CURRENT_BINARY_DIR}/dependencies/zookeeper/lib/libzookeeper_mt.so" CACHE STRING "" FORCE)
	include_directories(${ZOOKEEPER_INCLUDE_DIRS})
	set(ZOOKEEPER_FOUND TRUE CACHE STRING "" FORCE)
#else()
#	include_directories(${ZOOKEEPER_INCLUDE_DIRS})
#endif()
######################  PROTOBUF

#if (NOT PROTOBUF_FOUND)
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
#endif()


set(Boost_USE_MULTITHREADED ON)
#find_package(GTest REQUIRED)
find_package(Boost 1.55 REQUIRED COMPONENTS system thread iostreams serialization)
find_package(ZLIB REQUIRED) 

find_package (Threads)

include_directories(SYSTEM ${Boost_INCLUDE_DIR} )
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
file(GLOB_RECURSE EXTERN_SOURCES "src/data/extern/*cpp")
file(GLOB_RECURSE CWRAPPER_SOURCES "src/extern/*cpp")
file(GLOB_RECURSE INTERCONNECT_SOURCES "src/interconnect/*cpp")
file(GLOB_RECURSE SCANNER_SOURCES "src/scanner/*cpp")
file(GLOB_RECURSE WRITER_SOURCES "src/writer/*cpp")



add_library(sharkbite STATIC ${ZK_SOURCES} ${CONSTRUCT_SOURCES} ${STREAMING_SOURCES} ${CLIENT_SOURCES} ${EXCEPTION_SOURCES} ${EXTERN_SOURCES} ${INTERCONNECT_SOURCES} ${SCANNER_SOURCES} ${WRITER_SOURCES} ${CWRAPPER_SOURCES} )
add_library(sharkbite-shared SHARED ${ZK_SOURCES} ${CONSTRUCT_SOURCES} ${STREAMING_SOURCES} ${CLIENT_SOURCES} ${EXCEPTION_SOURCES} ${EXTERN_SOURCES} ${INTERCONNECT_SOURCES} ${SCANNER_SOURCES} ${WRITER_SOURCES} ${CWRAPPER_SOURCES} )

#if (NOT PROTOBUF_FOUND)
	add_dependencies(sharkbite Protobuf)
	add_dependencies(sharkbite-shared Protobuf)
#endif()
#if (NOT ZOOKEEPER_FOUND)
	add_dependencies(sharkbite ZooKeeper)
	add_dependencies(sharkbite-shared ZooKeeper)
#endif()
#if (NOT THRIFT_FOUND)
	add_dependencies(sharkbite LibThrift)
	add_dependencies(sharkbite-shared LibThrift)
#endif()
target_link_libraries (sharkbite ${ZLIB_LIBRARIES} ${CMAKE_THREAD_LIBS_INIT} ${ZOOKEEPER_LIBRARIES}   ${OPENSSL_LIBRARIES} ${THRIFT_LIBS} ${ZLIB_LIBRARIES} ${Boost_LIBRARIES})

target_link_libraries (sharkbite-shared ${ZLIB_LIBRARIES} ${CMAKE_THREAD_LIBS_INIT} ${ZOOKEEPER_SHARED_LIBRARIES}  ${OPENSSL_LIBRARIES}  ${THRIFT_SHARED_LIBS} ${ZLIB_LIBRARIES} ${Boost_LIBRARIES})


add_library(capi SHARED ${CAPI_SOURCES})
add_dependencies(capi sharkbite-shared)
target_link_libraries(capi PRIVATE sharkbite-shared)

#ClientExample
add_executable(ClientExample "src/examples/ClientExample.cpp")
target_link_libraries (ClientExample ${CMAKE_THREAD_LIBS_INIT})
target_link_libraries (ClientExample ${ZLIB_LIBRARIES})
target_link_libraries( ClientExample ${Boost_LIBRARIES} )
target_link_libraries( ClientExample ${ZOOKEEPER_LIBRARIES} ${OPENSSL_LIBRARIES})
target_link_libraries( ClientExample ${THRIFT_LIBS} sharkbite)

#ClientExample
add_executable(InsertTest "src/examples/InsertTest.cpp")
target_link_libraries (InsertTest ${CMAKE_THREAD_LIBS_INIT})
target_link_libraries (InsertTest ${ZLIB_LIBRARIES})
target_link_libraries( InsertTest ${Boost_LIBRARIES} )
target_link_libraries( InsertTest ${ZOOKEEPER_LIBRARIES} )
target_link_libraries( InsertTest ${THRIFT_LIBS} ${OPENSSL_LIBRARIES})
target_link_libraries( InsertTest sharkbite)


#ClientExample
add_executable(ContinuousIngest "src/examples/ContinuousIngest.cpp")
target_link_libraries (ContinuousIngest ${CMAKE_THREAD_LIBS_INIT})
target_link_libraries (ContinuousIngest ${ZLIB_LIBRARIES})
target_link_libraries( ContinuousIngest ${Boost_LIBRARIES} )
target_link_libraries( ContinuousIngest ${ZOOKEEPER_LIBRARIES} ${OPENSSL_LIBRARIES})
target_link_libraries( ContinuousIngest ${THRIFT_LIBS})
target_link_libraries( ContinuousIngest sharkbite)

#TableOps
add_executable(TableOps "src/examples/TableOps.cpp")
target_link_libraries (TableOps ${CMAKE_THREAD_LIBS_INIT})
target_link_libraries (TableOps ${ZLIB_LIBRARIES})
target_link_libraries( TableOps ${Boost_LIBRARIES} )
target_link_libraries( TableOps ${ZOOKEEPER_LIBRARIES} )
target_link_libraries( TableOps ${THRIFT_LIBS} ${OPENSSL_LIBRARIES})
target_link_libraries( TableOps sharkbite)

add_executable(RfileReadExample "src/examples/RfileReadExample.cpp")
target_link_libraries (RfileReadExample ${CMAKE_THREAD_LIBS_INIT})
target_link_libraries (RfileReadExample ${ZLIB_LIBRARIES})
target_link_libraries( RfileReadExample ${Boost_LIBRARIES} )
target_link_libraries( RfileReadExample ${ZOOKEEPER_LIBRARIES} ${OPENSSL_LIBRARIES} )
target_link_libraries( RfileReadExample ${THRIFT_LIBS})
target_link_libraries( RfileReadExample sharkbite)

add_executable(PrintInfo "src/examples/PrintInfo.cpp")
target_link_libraries (PrintInfo ${CMAKE_THREAD_LIBS_INIT})
target_link_libraries (PrintInfo ${ZLIB_LIBRARIES})
target_link_libraries( PrintInfo ${Boost_LIBRARIES} )
target_link_libraries( PrintInfo ${ZOOKEEPER_LIBRARIES} ${OPENSSL_LIBRARIES} )
target_link_libraries( PrintInfo ${THRIFT_LIBS})
target_link_libraries( PrintInfo sharkbite)

ADD_CUSTOM_TARGET(debian-package
	COMMAND dpkg-buildpackage -us -uc -b
	WORKING_DIRECTORY ${CMAKE_SOURCE_DIR}
	COMMENT "Create debian package..."
)


enable_testing()

  file(GLOB_RECURSE RFILE_TEST "test/constructs/rfile_test.cpp")
  add_executable(testrfile ${RFILE_TEST})
  target_link_libraries(testrfile  ${CMAKE_THREAD_LIBS_INIT} ${ZLIB_LIBRARIES} sharkbite)

  
  add_test(NAME testrfile
	   COMMAND testrfile)
	   
  file(GLOB_RECURSE TEST_CONSTRUCTS "test/constructs/TestConstructs.cpp")
  add_executable(testconstructs ${TEST_CONSTRUCTS})
  target_link_libraries(testconstructs  ${CMAKE_THREAD_LIBS_INIT} ${ZLIB_LIBRARIES}  sharkbite)

  
  add_test(NAME testconstructs
	   COMMAND testconstructs)
	   
  file(GLOB_RECURSE TEST_STREAMS "test/constructs/TestStreams.cpp")
  add_executable(teststreams ${TEST_STREAMS})
  target_link_libraries(teststreams  ${CMAKE_THREAD_LIBS_INIT} ${ZLIB_LIBRARIES} sharkbite)
  
  add_test(NAME teststreams
	   COMMAND teststreams)
	   
 
    file(GLOB_RECURSE TEST_ZK_INSTANCE "test/zookeeper/testInstance.cpp")
  add_executable(testzkInstance ${TEST_ZK_INSTANCE})
  target_link_libraries (testzkInstance ${CMAKE_THREAD_LIBS_INIT})
  target_link_libraries (testzkInstance ${ZLIB_LIBRARIES})
  target_link_libraries( testzkInstance ${Boost_LIBRARIES} )
  target_link_libraries( testzkInstance ${ZOOKEEPER_LIBRARIES} )
  target_link_libraries( testzkInstance ${OPENSSL_LIBRARIES} ${THRIFT_LIBS} sharkbite)

  target_link_libraries(testzkInstance sharkbite)
  
      file(GLOB_RECURSE VANDV "test/vandv/IntegrationTest.cpp")
  add_executable(integrationTest ${VANDV})
  target_link_libraries (integrationTest ${CMAKE_THREAD_LIBS_INIT})
  target_link_libraries (integrationTest ${ZLIB_LIBRARIES})
  target_link_libraries( integrationTest ${Boost_LIBRARIES} )
  target_link_libraries( integrationTest ${ZOOKEEPER_LIBRARIES} )
  target_link_libraries( integrationTest  ${THRIFT_LIBS} ${OPENSSL_LIBRARIES})
  target_link_libraries(integrationTest   sharkbite)



add_subdirectory(test/services)

add_subdirectory(include/interconnect/securityOps)

FIND_PACKAGE(Doxygen)
if(DOXYGEN_FOUND)
 
MESSAGE(Creating API Documentation)
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
