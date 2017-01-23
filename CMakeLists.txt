set(CMAKE_AUTOMOC ON)
cmake_minimum_required(VERSION 2.8.7)
project (Sharkbite)
set ( CMAKE_VERBOSE_MAKEFILE true )
set(CMAKE_BUILD_TYPE Debug)
#set (CMAKE_CXX_FLAGS "-std=c++11 -g -Wall -Wno-unused-value -Wno-unused-function -Wno-reorder")
set (CMAKE_CXX_FLAGS "-std=c++11 -O2 -Wall -Wno-unused-value -Wno-unused-function -Wno-reorder")
option(test "Build all tests." OFF) 
#Bring the headers, such as Student.h into the project
include_directories(include)


set(CMAKE_MODULE_PATH ${PROJECT_SOURCE_DIR}/cmake/modules)

set(Boost_USE_MULTITHREADED ON)
#let's get all them thar libraries
find_package(Thrift REQUIRED)
find_package(Zookeeper REQUIRED)
find_package(Hdfs3 REQUIRED)
find_package(Boost 1.55 REQUIRED COMPONENTS system thread iostreams serialization)
find_package(ZLIB REQUIRED)
find_package(GTest REQUIRED)
find_package (Threads)

include_directories(SYSTEM ${Boost_INCLUDE_DIR} )
include_directories(SYSTEM ${THRIFT_INCLUDE_DIR}/thrift )
include_directories(SYSTEM ${Zookeeper_INCLUDE_DIRS})
include_directories(SYSTEM ${HDFS3_INCLUDE_DIRS})
include_directories(SYSTEM ${ZLIB_INCLUDE_DIRS})
include_directories(SYSTEM ${GTEST_INCLUDE_DIRS})
message(STATUS "GTest include dir :  ${GTEST_INCLUDE_DIRS}")
message(STATUS "GTest lib : ${GTEST_LIBRARIES}")
message(STATUS "Thrift include dir: ${THRIFT_INCLUDE_DIR}/thrift")
message(STATUS "Thrift contrib dir: ${THRIFT_CONTRIB_DIR}")
message(STATUS "Thrift library path: ${THRIFT_LIBS}")
message(STATUS "Thrift static library: ${THRIFT_STATIC_LIB}")
message(STATUS "Thrift compiler: ${THRIFT_COMPILER}")
message(STATUS "ZK Include: ${Zookeeper_INCLUDE_DIRS}")
message(STATUS "ZK Lib: ${Zookeeper_LIBRARIES}")

message(STATUS "HDFS3 include dir: ${HDFS3_INCLUDE_DIR}")
message(STATUS "HFDS3 library path: ${HDFS3_LIBS}")
message(STATUS "HFDS3 Static library path: ${HDFS3_SHARED_LIB}")


#However, the file(GLOB...) allows for wildcard additions:
file(GLOB_RECURSE EXCEPTION_SOURCES "src/data/exceptions/*cpp")
file(GLOB_RECURSE CONSTRUCT_SOURCES "src/data/constructs/*cpp")
file(GLOB_RECURSE STREAMING_SOURCES "src/data/streaming/*cpp")
file(GLOB_RECURSE ZK_SOURCES "src/data/zookeeper/*cpp")
file(GLOB_RECURSE CLIENT_SOURCES "src/data/client/*cpp")
file(GLOB_RECURSE EXTERN_SOURCES "src/data/extern/*cpp")
file(GLOB_RECURSE INTERCONNECT_SOURCES "src/interconnect/*cpp")
file(GLOB_RECURSE SCANNER_SOURCES "src/scanner/*cpp")
file(GLOB_RECURSE WRITER_SOURCES "src/writer/*cpp")



#zk
add_library(errors SHARED ${EXCEPTION_SOURCES})
add_library(zkclient SHARED ${ZK_SOURCES} )
target_link_libraries( zkclient errors )
#target_link_libraries (zkclient ${CMAKE_THREAD_LIBS_INIT} ${Boost_LIBRARIES} ${THRIFT_LIB} ${Zookeeper_LIBRARIES} )
#constructs
add_library(constructs SHARED ${CONSTRUCT_SOURCES} )
target_link_libraries( constructs errors )
target_link_libraries( constructs ${ZLIB_LIBRARIES} )
#target_link_libraries (constructs ${CMAKE_THREAD_LIBS_INIT})
target_link_libraries( constructs ${Boost_LIBRARIES} )
#target_link_libraries( constructs ${Zookeeper_LIBRARIES} )
#target_link_libraries( constructs ${THRIFT_LIB} )
#streaming
add_library(streaming SHARED ${STREAMING_SOURCES})
target_link_libraries( streaming ${HDFS3_LIBS} )
#client
add_library(client SHARED ${CLIENT_SOURCES} ${EXCEPTION_SOURCES})
target_link_libraries (client errors zkclient constructs streaming )
#extern
add_library(extern SHARED ${EXTERN_SOURCES})
#interconnect
#add_library(interconnect SHARED ${INTERCONNECT_SOURCES})
#target_link_libraries(interconnect ${Boost_LIBRARIES})
#target_link_libraries(interconnect ${THRIFT_LIB})
#target_link_libraries(interconnect client)
#target_link_libraries(interconnect extern)
#scanner
add_library(scanner SHARED ${SCANNER_SOURCES})
target_link_libraries (scanner ${CMAKE_THREAD_LIBS_INIT})
target_link_libraries (scanner ${ZLIB_LIBRARIES})
target_link_libraries( scanner ${Boost_LIBRARIES} )
target_link_libraries( scanner ${Zookeeper_LIBRARIES} )
target_link_libraries( scanner ${THRIFT_LIB} )
target_link_libraries(scanner client)
target_link_libraries(scanner extern)
#target_link_libraries(scanner interconnect)
#writer
add_library(writer SHARED ${WRITER_SOURCES})
target_link_libraries (writer ${ZLIB_LIBRARIES})
target_link_libraries( writer ${Boost_LIBRARIES} )
target_link_libraries(writer client)
target_link_libraries(writer extern)
#target_link_libraries( writer ${THRIFT_LIB} )
#target_link_libraries (writer ${CMAKE_THREAD_LIBS_INIT})
#target_link_libraries( writer ${Boost_LIBRARIES} )
#target_link_libraries(writer interconnect)

#interconnect
add_library(interconnect SHARED ${INTERCONNECT_SOURCES})
target_link_libraries(interconnect ${Boost_LIBRARIES})
target_link_libraries(interconnect ${THRIFT_LIB})
target_link_libraries(interconnect client)
target_link_libraries(interconnect extern)
target_link_libraries(interconnect writer)
target_link_libraries(interconnect scanner)


#ClientExample
add_executable(ClientExample "src/examples/ClientExample.cpp")
target_link_libraries (ClientExample ${CMAKE_THREAD_LIBS_INIT})
target_link_libraries (ClientExample ${ZLIB_LIBRARIES})
target_link_libraries( ClientExample ${Boost_LIBRARIES} )
target_link_libraries( ClientExample ${Zookeeper_LIBRARIES} )
target_link_libraries( ClientExample ${THRIFT_LIB})
target_link_libraries( ClientExample ${HDFS3_LIBS} errors constructs streaming scanner writer interconnect)

#ClientExample
add_executable(InsertTest "src/examples/InsertTest.cpp")
target_link_libraries (InsertTest ${CMAKE_THREAD_LIBS_INIT})
target_link_libraries (InsertTest ${ZLIB_LIBRARIES})
target_link_libraries( InsertTest ${Boost_LIBRARIES} )
target_link_libraries( InsertTest ${Zookeeper_LIBRARIES} )
target_link_libraries( InsertTest ${THRIFT_LIB})
target_link_libraries( InsertTest ${HDFS3_LIBS} errors constructs streaming scanner writer interconnect)


#ClientExample
add_executable(ContinuousIngest "src/examples/ContinuousIngest.cpp")
target_link_libraries (ContinuousIngest ${CMAKE_THREAD_LIBS_INIT})
target_link_libraries (ContinuousIngest ${ZLIB_LIBRARIES})
target_link_libraries( ContinuousIngest ${Boost_LIBRARIES} )
target_link_libraries( ContinuousIngest ${Zookeeper_LIBRARIES} )
target_link_libraries( ContinuousIngest ${THRIFT_LIB})
target_link_libraries( ContinuousIngest ${HDFS3_LIBS} errors constructs streaming scanner writer interconnect)


#TableOps
add_executable(TableOps "src/examples/TableOps.cpp")
target_link_libraries (TableOps ${CMAKE_THREAD_LIBS_INIT})
target_link_libraries (TableOps ${ZLIB_LIBRARIES})
target_link_libraries( TableOps ${Boost_LIBRARIES} )
target_link_libraries( TableOps ${Zookeeper_LIBRARIES} )
target_link_libraries( TableOps ${THRIFT_LIB})
target_link_libraries( TableOps ${HDFS3_LIBS} errors constructs streaming scanner writer interconnect)


add_executable(RfileReadExample "src/examples/RfileReadExample.cpp")
target_link_libraries (RfileReadExample ${CMAKE_THREAD_LIBS_INIT})
target_link_libraries (RfileReadExample ${ZLIB_LIBRARIES})
target_link_libraries( RfileReadExample ${Boost_LIBRARIES} )
target_link_libraries( RfileReadExample ${Zookeeper_LIBRARIES} )
target_link_libraries( RfileReadExample ${THRIFT_LIB})
target_link_libraries( RfileReadExample ${HDFS3_LIBS} errors constructs streaming scanner writer interconnect )

add_executable(PrintInfo "src/examples/PrintInfo.cpp")
target_link_libraries (PrintInfo ${CMAKE_THREAD_LIBS_INIT})
target_link_libraries (PrintInfo ${ZLIB_LIBRARIES})
target_link_libraries( PrintInfo ${Boost_LIBRARIES} )
target_link_libraries( PrintInfo ${Zookeeper_LIBRARIES} )
target_link_libraries( PrintInfo ${THRIFT_LIB})
target_link_libraries( PrintInfo ${HDFS3_LIBS} errors constructs streaming scanner writer interconnect )


enable_testing()

  file(GLOB_RECURSE RFILE_TEST "test/constructs/rfile_test.cpp")
  add_executable(testrfile ${RFILE_TEST})
  target_link_libraries(testrfile  ${CMAKE_THREAD_LIBS_INIT} ${ZLIB_LIBRARIES})

  target_link_libraries(testrfile ${HDFS3_LIBS} constructs streaming)
  
  add_test(NAME testrfile
	   COMMAND testrfile)
	   
  file(GLOB_RECURSE TEST_CONSTRUCTS "test/constructs/TestConstructs.cpp")
  add_executable(testconstructs ${TEST_CONSTRUCTS})
  target_link_libraries(testconstructs  ${CMAKE_THREAD_LIBS_INIT} ${ZLIB_LIBRARIES})

  target_link_libraries(testconstructs ${HDFS3_LIBS} constructs streaming)
  
  add_test(NAME testconstructs
	   COMMAND testconstructs)
	   
  file(GLOB_RECURSE TEST_STREAMS "test/constructs/TestStreams.cpp")
  add_executable(teststreams ${TEST_STREAMS})
  target_link_libraries(teststreams  ${CMAKE_THREAD_LIBS_INIT} ${ZLIB_LIBRARIES})

  target_link_libraries(teststreams ${HDFS3_LIBS} constructs streaming)
  
  add_test(NAME teststreams
	   COMMAND teststreams)
	   
 
    file(GLOB_RECURSE TEST_ZK_INSTANCE "test/zookeeper/testInstance.cpp")
  add_executable(testzkInstance ${TEST_ZK_INSTANCE})
  target_link_libraries (testzkInstance ${CMAKE_THREAD_LIBS_INIT})
  target_link_libraries (testzkInstance ${ZLIB_LIBRARIES})
  target_link_libraries( testzkInstance ${Boost_LIBRARIES} )
  target_link_libraries( testzkInstance ${Zookeeper_LIBRARIES} )
  target_link_libraries( testzkInstance ${HDFS3_LIBS} ${THRIFT_LIB})

  target_link_libraries(testzkInstance client)
  
      file(GLOB_RECURSE VANDV "test/vandv/IntegrationTest.cpp")
  add_executable(integrationTest ${VANDV})
  target_link_libraries (integrationTest ${CMAKE_THREAD_LIBS_INIT})
  target_link_libraries (integrationTest ${ZLIB_LIBRARIES})
  target_link_libraries( integrationTest ${Boost_LIBRARIES} )
  target_link_libraries( integrationTest ${Zookeeper_LIBRARIES} )
  target_link_libraries( integrationTest ${HDFS3_LIBS} ${THRIFT_LIB})

  target_link_libraries(integrationTest  ${HDFS3_LIBS} errors constructs streaming scanner writer interconnect)



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
