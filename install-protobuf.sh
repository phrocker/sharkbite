#!/bin/bash
LDCONF=`ldconfig -p`
if [[ "$LDCONF" == *"libprotobuf"* ]]
then
	echo "Lib protobuf already installed";
else
	set -ex
	rm -rf protobuf*
	wget https://github.com/google/protobuf/archive/2.7.0.zip -O protobuf-2.7.0.zip
	unzip protobuf-2.7.0.zip
	cd protobuf-2.7.0 && ./autogen.sh && ./configure --prefix=/usr && make && sudo make install && sudo ldconfig
fi
