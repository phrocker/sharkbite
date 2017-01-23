CPPFLAGS=-std=c++11 -lz -lboost_iostreams -lzookeeper_mt  -I/usr/local/include/thrift/ -I /usr/local/include/zookeeper -lpthread -lthrift -lhdfs3 
LDFLAGS=-g ../../../../extern/thrift/thrift.so  -L/usr/local/lib/  ../../client.so ../zookeeper.so ../../../constructs.so ../../../configuration/Configuration.so ../../../../streaming/streaming.so ../../../../../interconnect/interconnect.so -Wl,-rpath=/usr/local/lib/
LDLIBS=
RM=rm -f
PWD=`pwd`

SRCS=testInstance.cpp
OBJS=$(subst .cpp,.o,$(SRCS))



all: test

clean: 
	$(RM) $(OBJS)


test:	$(OBJS)
	#cd ../ ; make clean; make ; cd $(PWD)
	#cd ../../ ; make clean; make ; cd $(PWD)
	g++ -o construct_test $(OBJS) $(LDFLAGS) $(CPPFLAGS)
	chmod 755 construct_test
	$(RM) $(OBJS)
	./construct_test

	
testInstance: testInstance.cpp
	g++ -o testInstance  $(LDFLAGS)  $(CPPFLAGS) testInstance.cpp
	chmod 755 testInstance 
	$(RM) $(OBJS)
	./testInstance
	
