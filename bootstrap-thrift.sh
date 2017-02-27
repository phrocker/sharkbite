#!/bin/bash
if [ ! -d "thrift-0.9.3" ]; then
rm -rf thrift*.gz
wget http://archive.apache.org/dist/thrift/0.9.3/thrift-0.9.3.tar.gz -O thrift-0.9.3.tar.gz
tar -xzvf thrift-0.9.3.tar.gz
fi
pushd thrift-0.9.3 && ./configure --without-qt4  --without-c_glib  --without-csharp --without-java --without-erlang --without-nodejs --without-lua --without-python --without-perl --without-php --without-php_extension --without-ruby --without-haskell --without-go --without-d --prefix=/usr && make && sudo make install
popd
if [ -f "thrift-0.9.3.tar.gz" ]; then
rm thrift-0.9.3.tar.gz
fi
