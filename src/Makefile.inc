# put common definitions in here
CC=g++
LD=g++
LDFLAGS=-lthrift -lboost_system  -lboost_iostreams -lz -lpthread -lboost_thread  -lzookeeper_mt  -lpthread -lhdfs3 -Wl,-rpath=/usr/local/lib/
AR=ar
ARFLAGS	=
MAKE=make
SUBLIB=./interconnect/accumulo/interconnect-accumulo.so ./interconnect/tableOps/tableops.so ./interconnect/transport/transport.so ./interconnect/interconnect.so ./interconnect/scanrequest/scanrequest.so ./scanner/scanner.so  ./scanner/constructs/scannerconstructs.so ./data/streaming/streaming.so ./data/extern/thrift/thrift.so ./data/constructs/server/server.so ./data/constructs/configuration/configuration.so ./data/constructs/rfile/bcfile/bcfile.so ./data/constructs/rfile/meta/meta.so ./data/constructs/rfile/test/libhdfs3.so ./data/constructs/rfile/rfile.so ./data/constructs/constructs.so ./data/constructs/compressor/compressors.so ./data/constructs/security/security.so ./data/constructs/client/client.so ./data/constructs/client/zookeeper/zookeeper.so ./data/client/data-client.so ./data/data.so writer/impl/writer.so
RANLIB=ranlib
RM=rm
ECHO=echo

SHELL=/bin/sh

#.SILENT :
