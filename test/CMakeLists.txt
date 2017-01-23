CMAKE_MINIMUM_REQUIRED(VERSION 2.8.7)
set (CMAKE_CXX_FLAGS "-std=c++11")

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

enable_testing()

include_directories(${GTEST_INCLUDE_DIRS})

add_executable(runUnitTests constructs/TestColumn.cpp)
target_link_libraries(runUnitTests ${GTEST_BOTH_LIBRARIES})
add_test(runUnitTests runUnitTests)

#target_link_libraries(runUnitTests gtest gtest_main)

#target_link_libraries(runUnitTests example)

message(STATUS "Tests Built")
