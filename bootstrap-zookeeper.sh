#!/bin/bash
if [ ! -d "zookeeper-3.4.8" ]; then
rm -rf zookeeper*.gz
wget http://mirrors.gigenet.com/apache/zookeeper/zookeeper-3.4.8/zookeeper-3.4.8.tar.gz -O zookeeper-3.4.8.tar.gz
tar -xzvf zookeeper-3.4.8.tar.gz
fi
pushd zookeeper-3.4.8/src/c && ./configure --prefix=${pwd} && make
popd
if [ -f "zookeeper-3.4.8.tar.gz" ]; then
rm zookeeper-3.4.8.tar.gz
fi