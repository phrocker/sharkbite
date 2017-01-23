#!/bin/bash
LDCONF=`ldconfig -p`
if [[ "$LDCONF" == *"libzookeeper"* ]]
then
        echo "Lib zookeeper already installed";
else
        set -ex
	rm -rf zookeeper*
	wget http://mirrors.gigenet.com/apache/zookeeper/zookeeper-3.4.8/zookeeper-3.4.8.tar.gz -O zookeeper-3.4.8.tar.gz
	tar -xzvf zookeeper-3.4.8.tar.gz
	cd zookeeper-3.4.8/src/c && ./configure --prefix=/usr && make && sudo make install
fi
