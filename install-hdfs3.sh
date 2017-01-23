#!/bin/bash
LDCONF=`ldconfig -p`
if [[ "$LDCONF" == *"libhdfs"* ]]
then
        echo "Lib hdfs already installed";
else
        set -ex
	rm -rf apache*
	wget https://github.com/PivotalRD/libhdfs3/archive/apache-rpc-9.zip -O apache-rpc-9.zip
	unzip apache-rpc-9.zip
	cd pivotalrd-libhdfs3-apache-rpc-9 && mkdir build && cd build && cmake ../ && make && sudo make install
fi
