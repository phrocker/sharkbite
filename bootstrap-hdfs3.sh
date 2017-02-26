#!/bin/bash
rm -rf apache*
if [ ! -d "pivotalrd-libhdfs3-apache-rpc-9" ]; then
wget https://github.com/Pivotal-Data-Attic/pivotalrd-libhdfs3/archive/v2.2.31.zip -O apache-rpc-9.zip
unzip apache-rpc-9.zip
fi
pushd pivotalrd-libhdfs3-apache-rpc-9 && mkdir build && cd build && cmake ../ && make
popd
if [ -f "apache-rpc-9.zip" ]; then
rm apache-rpc-9.zip
fi
