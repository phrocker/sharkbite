#!/bin/bash
rm -rf gtest*
wget https://github.com/google/googletest/archive/master.zip -O gtest.zip
unzip gtest.zip
pushd googletest-master && mkdir build && cd build && cmake -DBUILD_SHARED_LIBS=ON ../ && make
popd
rm gtest.zip
