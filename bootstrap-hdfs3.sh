#!/bin/bash
rm -rf apache*
wget https://github.com/PivotalRD/libhdfs3/archive/apache-rpc-9.zip -O apache-rpc-9.zip
unzip apache-rpc-9.zip
pushd pivotalrd-libhdfs3-apache-rpc-9 && mkdir build && cd build && cmake ../ && make
popd
rm apache-rpc-9.zip
