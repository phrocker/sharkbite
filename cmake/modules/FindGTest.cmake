
if(EXISTS ${CMAKE_ROOT}/Modules/FindGTest.cmake)
	include(${CMAKE_ROOT}/Modules/FindZLIB.cmake)

	if(GTEST_FOUND)
		return()
	endif()
	endif()

find_path(GTEST_INCLUDE_DIRS gtest/gtest.h HINTS
  ${CMAKE_CURRENT_SOURCE_DIR}/dependencies/googletest-release-1.8.0/googletest/include/
)
message(STATUS "GTEST_INCLUDE_DIRS: ${GTEST_INCLUDE_DIRS}")

set(GTEST_LIBRARIES_PATHS
  ${CMAKE_CURRENT_SOURCE_DIR}/dependencies/googletest-release-1.8.0/build/googlemock/gtest/)


find_library(GTEST_LIB NAMES gtest HINTS ${GTEST_LIBRARIES_PATHS})

message (STATUS "GTEST_LIBRARIES: ${GTEST_LIB}")

if (GTEST_LIB)
  set(GTEST_FOUND TRUE)
  set(GTEST_LIBRARIES ${GTEST_LIBR})
else ()
  set(GTEST_FOUND FALSE)
endif ()


mark_as_advanced(
  GTEST_LIBRARIES
  GTEST_INCLUDE_DIRS
)
