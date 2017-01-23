#!/bin/bash
LDCONF=`ldconfig -p`
if [[ "$LDCONF" == *"libgtest"* ]]
then
        echo "Lib gtest already installed";
else
        set -ex
	rm -rf gtest*
	wget https://github.com/google/googletest/archive/master.zip -O gtest.zip
	unzip gtest.zip
	cd googletest-master && mkdir build && cd build && cmake -DBUILD_SHARED_LIBS=ON ../ && make && sudo make install
fi
