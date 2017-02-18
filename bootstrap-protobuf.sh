#!/bin/bash
rm -rf protobuf*
wget https://github.com/google/protobuf/archive/master.zip -O protobuf-2.7.0.zip
unzip protobuf-2.7.0.zip
pushd protobuf-master && ./autogen.sh && ./configure --prefix=${pwd} && make
rm protobuf-2.7.0.zip
