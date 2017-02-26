#!/bin/bash
rm -rf gtest*
if [ ! -d "googletest-release-1.8.0" ]; then
wget https://github.com/google/googletest/archive/release-1.8.0.zip -O gtest.zip
unzip gtest.zip
fi
pushd googletest-release-1.8.0 && mkdir build && cd build && cmake -DBUILD_SHARED_LIBS=ON ../ && make
popd
if [ -f gtest.zip ]; then
rm gtest.zip
fi
