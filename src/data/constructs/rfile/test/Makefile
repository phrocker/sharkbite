CPPFLAGS=-std=c++11
LDFLAGS=-O3 -L/usr/local/lib/ ../../../extern/thrift/thrift.so ../../../streaming/streaming.so ../../compressor/compressors.so ../../constructs.so ../bcfile/bcfile.so ../meta/meta.so ../rfile.so -lz -lthrift -I/usr/local/include/thrift/ -lhdfs3
LDLIBS=
RM=rm -f
PWD=`pwd`

SRCS=rfile_test.cpp
OBJS=$(subst .cpp,.o,$(SRCS))



all: test

clean: 
	$(RM) $(OBJS)


test:	$(OBJS)
#	cd ../../../streaming ; make clean; make ; cd $(PWD)
#	cd ../../compressor ; make clean; make ; cd $(PWD)
#	cd ../../ ; make clean; make ; cd $(PWD)
#	cd ../meta/ ; make clean; make ; cd $(PWD)
#	cd ../bcfile/ ; make clean; make ; cd $(PWD)
#	cd ../ ; make clean; make ; cd $(PWD)
	g++ -o construct_test $(OBJS) $(LDFLAGS) $(CPPFLAGS)
	chmod 755 construct_test
	$(RM) $(OBJS)
	./construct_test

	
