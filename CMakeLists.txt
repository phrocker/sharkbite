set(CMAKE_AUTOMOC ON)
cmake_minimum_required(VERSION 2.8.7)
project (Sharkbite)
set ( CMAKE_VERBOSE_MAKEFILE true )
set (CMAKE_CXX_FLAGS "-std=c++11 -g -Wall -fomit-frame-pointer -fno-stack-protector -Wno-unused-value -Wno-unused-function -Wno-reorder -Wno-overloaded-virtual -Wno-char-subscripts -Wno-tautological-constant-out-of-range-compare -Wno-unknown-pragmas ")
#  -fsingle-precision-constant 
option(test "Build all tests." OFF) 
include_directories(include)


set(CMAKE_MODULE_PATH ${PROJECT_SOURCE_DIR}/cmake/modules)

set(Boost_USE_MULTITHREADED ON)
find_package(GTest REQUIRED)
#let's get all them thar libraries
find_package(Thrift REQUIRED)
find_package(Zookeeper REQUIRED)
find_package(Boost 1.55 REQUIRED COMPONENTS system thread iostreams serialization)
find_package(ZLIB REQUIRED) 

find_package (Threads)

include_directories(SYSTEM ${Boost_INCLUDE_DIR} )
include_directories(SYSTEM ${THRIFT_INCLUDE_DIR}/thrift )
include_directories(SYSTEM ${Zookeeper_INCLUDE_DIRS})
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


#However, the file(GLOB...) allows for wildcard additions:
file(GLOB_RECURSE EXCEPTION_SOURCES "src/data/exceptions/*cpp")
file(GLOB_RECURSE CONSTRUCT_SOURCES "src/data/constructs/*cpp")
file(GLOB_RECURSE STREAMING_SOURCES "src/data/streaming/*cpp")
file(GLOB_RECURSE ZK_SOURCES "src/data/zookeeper/*cpp")
file(GLOB_RECURSE CLIENT_SOURCES "src/data/client/*cpp")
file(GLOB_RECURSE EXTERN_SOURCES "src/data/extern/*cpp")
file(GLOB_RECURSE CWRAPPER_SOURCES "src/extern/*cpp")
file(GLOB_RECURSE INTERCONNECT_SOURCES "src/interconnect/*cpp")
file(GLOB_RECURSE SCANNER_SOURCES "src/scanner/*cpp")
file(GLOB_RECURSE WRITER_SOURCES "src/writer/*cpp")



add_library(sharkbite STATIC ${ZK_SOURCES} ${CONSTRUCT_SOURCES} ${STREAMING_SOURCES} ${CLIENT_SOURCES} ${EXCEPTION_SOURCES} ${EXTERN_SOURCES} ${INTERCONNECT_SOURCES} ${SCANNER_SOURCES} ${WRITER_SOURCES} ${CWRAPPER_SOURCES} )
add_library(sharkbite-shared SHARED ${ZK_SOURCES} ${CONSTRUCT_SOURCES} ${STREAMING_SOURCES} ${CLIENT_SOURCES} ${EXCEPTION_SOURCES} ${EXTERN_SOURCES} ${INTERCONNECT_SOURCES} ${SCANNER_SOURCES} ${WRITER_SOURCES} ${CWRAPPER_SOURCES} )

target_link_libraries (sharkbite ${ZLIB_LIBRARIES} ${CMAKE_THREAD_LIBS_INIT} ${Zookeeper_LIBRARIES}   ${THRIFT_LIB} ${ZLIB_LIBRARIES} ${Boost_LIBRARIES}   )
target_link_libraries (sharkbite-shared  ${ZLIB_LIBRARIES} ${CMAKE_THREAD_LIBS_INIT} ${Zookeeper_LIBRARIES}   ${THRIFT_LIB} ${ZLIB_LIBRARIES} ${Boost_LIBRARIES}   )


#ClientExample
add_executable(ClientExample "src/examples/ClientExample.cpp")
target_link_libraries (ClientExample ${CMAKE_THREAD_LIBS_INIT})
target_link_libraries (ClientExample ${ZLIB_LIBRARIES})
target_link_libraries( ClientExample ${Boost_LIBRARIES} )
target_link_libraries( ClientExample ${Zookeeper_LIBRARIES} )
target_link_libraries( ClientExample ${THRIFT_LIB} sharkbite)

#ClientExample
add_executable(InsertTest "src/examples/InsertTest.cpp")
target_link_libraries (InsertTest ${CMAKE_THREAD_LIBS_INIT})
target_link_libraries (InsertTest ${ZLIB_LIBRARIES})
target_link_libraries( InsertTest ${Boost_LIBRARIES} )
target_link_libraries( InsertTest ${Zookeeper_LIBRARIES} )
target_link_libraries( InsertTest ${THRIFT_LIB})
target_link_libraries( InsertTest sharkbite)


#ClientExample
add_executable(ContinuousIngest "src/examples/ContinuousIngest.cpp")
target_link_libraries (ContinuousIngest ${CMAKE_THREAD_LIBS_INIT})
target_link_libraries (ContinuousIngest ${ZLIB_LIBRARIES})
target_link_libraries( ContinuousIngest ${Boost_LIBRARIES} )
target_link_libraries( ContinuousIngest ${Zookeeper_LIBRARIES} )
target_link_libraries( ContinuousIngest ${THRIFT_LIB})
target_link_libraries( ContinuousIngest sharkbite)

#TableOps
add_executable(TableOps "src/examples/TableOps.cpp")
target_link_libraries (TableOps ${CMAKE_THREAD_LIBS_INIT})
target_link_libraries (TableOps ${ZLIB_LIBRARIES})
target_link_libraries( TableOps ${Boost_LIBRARIES} )
target_link_libraries( TableOps ${Zookeeper_LIBRARIES} )
target_link_libraries( TableOps ${THRIFT_LIB})
target_link_libraries( TableOps sharkbite)

add_executable(RfileReadExample "src/examples/RfileReadExample.cpp")
target_link_libraries (RfileReadExample ${CMAKE_THREAD_LIBS_INIT})
target_link_libraries (RfileReadExample ${ZLIB_LIBRARIES})
target_link_libraries( RfileReadExample ${Boost_LIBRARIES} )
target_link_libraries( RfileReadExample ${Zookeeper_LIBRARIES} )
target_link_libraries( RfileReadExample ${THRIFT_LIB})
target_link_libraries( RfileReadExample sharkbite)

add_executable(PrintInfo "src/examples/PrintInfo.cpp")
target_link_libraries (PrintInfo ${CMAKE_THREAD_LIBS_INIT})
target_link_libraries (PrintInfo ${ZLIB_LIBRARIES})
target_link_libraries( PrintInfo ${Boost_LIBRARIES} )
target_link_libraries( PrintInfo ${Zookeeper_LIBRARIES} )
target_link_libraries( PrintInfo ${THRIFT_LIB})
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
  target_link_libraries( testzkInstance ${Zookeeper_LIBRARIES} )
  target_link_libraries( testzkInstance ${THRIFT_LIB} sharkbite)

  target_link_libraries(testzkInstance sharkbite)
  
      file(GLOB_RECURSE VANDV "test/vandv/IntegrationTest.cpp")
  add_executable(integrationTest ${VANDV})
  target_link_libraries (integrationTest ${CMAKE_THREAD_LIBS_INIT})
  target_link_libraries (integrationTest ${ZLIB_LIBRARIES})
  target_link_libraries( integrationTest ${Boost_LIBRARIES} )
  target_link_libraries( integrationTest ${Zookeeper_LIBRARIES} )
  target_link_libraries( integrationTest  ${THRIFT_LIB})
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
install(TARGETS sharkbite-shared
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
