#!/bin/bash
if [ ! -d "protobuf-3.1.0" ]; then
rm -rf protobuf*
wget https://github.com/google/protobuf/archive/v3.1.0.zip -O protobuf.zip
unzip protobuf.zip
fi
pushd protobuf-3.1.0 && ./autogen.sh && ./configure --prefix=${pwd} && make && sudo make install
if [ -f "protobuf.zip" ]; then
rm protobuf.zip
fi