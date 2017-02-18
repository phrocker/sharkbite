#!/bin/bash
rm -rf zookeeper*
wget http://mirrors.gigenet.com/apache/zookeeper/zookeeper-3.4.8/zookeeper-3.4.8.tar.gz -O zookeeper-3.4.8.tar.gz
tar -xzvf zookeeper-3.4.8.tar.gz
pushd zookeeper-3.4.8/src/c && ./configure --prefix=${pwd} && make
rm zookeeper-3.4.8.tar.gz
